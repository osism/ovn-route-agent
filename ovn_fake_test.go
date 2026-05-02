package main

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/ovn-kubernetes/libovsdb/cache"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// fakeOVSDBClient is a hand-rolled stand-in for the libovsdb client used by
// ovn_gateway.go. It serves List() from in-memory rows keyed by table name and
// records every transaction so tests can assert which OVSDB operations the
// production code emitted, without round-tripping through a real server.
//
// The fake intentionally does NOT apply mutations to its in-memory rows:
// each test sets the rows it wants the production code to see, then inspects
// transacts to verify the right ops were issued. This keeps the fake small
// while still exercising every write path in ovn_gateway.go.
type fakeOVSDBClient struct {
	dbm model.ClientDBModel

	mu        sync.Mutex
	rows      map[string][]model.Model
	transacts [][]ovsdb.Operation

	transactErr error
	listErr     error

	// onTransact is invoked synchronously for each Transact call (after the
	// op has been recorded). Used by drain tests to mutate rows mid-poll so
	// countLocalCRPorts can converge.
	onTransact func()
}

func newFakeOVSDBClient(dbm model.ClientDBModel) *fakeOVSDBClient {
	return &fakeOVSDBClient{
		dbm:  dbm,
		rows: make(map[string][]model.Model),
	}
}

// setRows replaces the in-memory rows for the given table. Accepts any
// pointer-to-model so call sites can pass concrete fixture types directly
// (e.g. *NBLogicalRouter) without boilerplate slice conversion.
func (f *fakeOVSDBClient) setRows(table string, rows ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(rows) == 0 {
		delete(f.rows, table)
		return
	}
	models := make([]model.Model, 0, len(rows))
	for _, r := range rows {
		models = append(models, r)
	}
	f.rows[table] = models
}

func (f *fakeOVSDBClient) recordedTransacts() [][]ovsdb.Operation {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]ovsdb.Operation, len(f.transacts))
	for i, ops := range f.transacts {
		out[i] = append([]ovsdb.Operation(nil), ops...)
	}
	return out
}

func (f *fakeOVSDBClient) tableForType(t reflect.Type) string {
	for table, mt := range f.dbm.Types() {
		if mt == t {
			return table
		}
	}
	return ""
}

// uuidOfModel reads the UUID field from a struct via reflection.
func uuidOfModel(m model.Model) string {
	v := reflect.ValueOf(m)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	f := v.FieldByName("UUID")
	if !f.IsValid() || f.Kind() != reflect.String {
		return ""
	}
	return f.String()
}

// --- ovsdbClient surface -----------------------------------------------------

func (f *fakeOVSDBClient) Connect(context.Context) error { return nil }
func (f *fakeOVSDBClient) Close()                        {}
func (f *fakeOVSDBClient) Cache() *cache.TableCache      { return nil }

func (f *fakeOVSDBClient) NewMonitor(_ ...client.MonitorOption) *client.Monitor {
	return &client.Monitor{}
}

func (f *fakeOVSDBClient) Monitor(context.Context, *client.Monitor) (client.MonitorCookie, error) {
	return client.MonitorCookie{}, nil
}

func (f *fakeOVSDBClient) List(_ context.Context, result interface{}) error {
	if f.listErr != nil {
		return f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	out := reflect.ValueOf(result).Elem()
	if out.Kind() != reflect.Slice {
		return fmt.Errorf("fakeOVSDBClient.List: result must be pointer to slice, got %T", result)
	}
	elemType := out.Type().Elem()
	ptrType := reflect.PointerTo(elemType)

	table := f.tableForType(ptrType)
	if table == "" {
		return fmt.Errorf("fakeOVSDBClient.List: no table mapping for %s", elemType)
	}

	rows := f.rows[table]
	slice := reflect.MakeSlice(out.Type(), len(rows), len(rows))
	for i, r := range rows {
		slice.Index(i).Set(reflect.ValueOf(r).Elem())
	}
	out.Set(slice)
	return nil
}

func (f *fakeOVSDBClient) Create(models ...model.Model) ([]ovsdb.Operation, error) {
	ops := make([]ovsdb.Operation, 0, len(models))
	for _, m := range models {
		ops = append(ops, ovsdb.Operation{
			Op:       ovsdb.OperationInsert,
			Table:    f.tableForType(reflect.TypeOf(m)),
			UUIDName: uuidOfModel(m),
		})
	}
	return ops, nil
}

func (f *fakeOVSDBClient) Where(models ...model.Model) client.ConditionalAPI {
	var target model.Model
	if len(models) > 0 {
		target = models[0]
	}
	return &fakeConditionalAPI{client: f, target: target}
}

func (f *fakeOVSDBClient) Transact(_ context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	f.mu.Lock()
	f.transacts = append(f.transacts, append([]ovsdb.Operation(nil), ops...))
	hook := f.onTransact
	f.mu.Unlock()

	if hook != nil {
		hook()
	}
	if f.transactErr != nil {
		return nil, f.transactErr
	}
	results := make([]ovsdb.OperationResult, len(ops))
	return results, nil
}

// --- ConditionalAPI surface --------------------------------------------------

type fakeConditionalAPI struct {
	client *fakeOVSDBClient
	target model.Model
}

func (c *fakeConditionalAPI) List(context.Context, any) error { return nil }

func (c *fakeConditionalAPI) Mutate(model.Model, ...model.Mutation) ([]ovsdb.Operation, error) {
	return nil, nil
}

// Update returns a synthetic op whose Row maps each selected field's
// ovsdb column tag to the field's current value, so tests can inspect
// what the production code is about to write (e.g. a boosted priority).
// Field pointers are matched to struct fields by address; if no fields
// are passed, all ovsdb-tagged fields are included.
func (c *fakeConditionalAPI) Update(m model.Model, fields ...any) ([]ovsdb.Operation, error) {
	row := ovsdb.Row{}
	v := reflect.ValueOf(m)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil, fmt.Errorf("fakeConditionalAPI.Update: expected pointer to struct, got %T", m)
	}
	if len(fields) == 0 {
		for i := 0; i < v.NumField(); i++ {
			tag := v.Type().Field(i).Tag.Get("ovsdb")
			if tag == "" || tag == "_uuid" {
				continue
			}
			row[tag] = v.Field(i).Interface()
		}
	} else {
		for _, fp := range fields {
			fpv := reflect.ValueOf(fp)
			if fpv.Kind() != reflect.Pointer {
				continue
			}
			fpAddr := fpv.Pointer()
			for i := 0; i < v.NumField(); i++ {
				if v.Field(i).CanAddr() && v.Field(i).Addr().Pointer() == fpAddr {
					tag := v.Type().Field(i).Tag.Get("ovsdb")
					if tag != "" && tag != "_uuid" {
						row[tag] = v.Field(i).Interface()
					}
					break
				}
			}
		}
	}
	return []ovsdb.Operation{{
		Op:    ovsdb.OperationUpdate,
		Table: c.client.tableForType(reflect.TypeOf(m)),
		UUID:  uuidOfModel(m),
		Row:   row,
	}}, nil
}

func (c *fakeConditionalAPI) Delete() ([]ovsdb.Operation, error) {
	if c.target == nil {
		return nil, nil
	}
	return []ovsdb.Operation{{
		Op:    ovsdb.OperationDelete,
		Table: c.client.tableForType(reflect.TypeOf(c.target)),
		UUID:  uuidOfModel(c.target),
	}}, nil
}

func (c *fakeConditionalAPI) Wait(ovsdb.WaitCondition, *int, model.Model, ...any) ([]ovsdb.Operation, error) {
	return nil, nil
}

// Compile-time assertions.
var (
	_ ovsdbClient           = (*fakeOVSDBClient)(nil)
	_ client.ConditionalAPI = (*fakeConditionalAPI)(nil)
)

// newOVNClientWithFakes returns an OVNClient wired to two fakeOVSDBClient
// instances built from the production NB/SB schemas. The returned OVNClient
// has its LocalChassisName preset for tests that read it.
func newOVNClientWithFakes(t testingT, localChassis string) (*OVNClient, *fakeOVSDBClient, *fakeOVSDBClient) {
	t.Helper()
	nbm, err := nbDatabaseModel()
	if err != nil {
		t.Fatalf("nbDatabaseModel: %v", err)
	}
	sbm, err := sbDatabaseModel()
	if err != nil {
		t.Fatalf("sbDatabaseModel: %v", err)
	}
	nb := newFakeOVSDBClient(nbm)
	sb := newFakeOVSDBClient(sbm)
	c := NewOVNClient(Config{}, nil)
	c.nbClient = nb
	c.sbClient = sb
	c.state.LocalChassisName = localChassis
	return c, nb, sb
}

// testingT is a tiny shim so the helper can be used from both *testing.T and
// *testing.B without importing testing here.
type testingT interface {
	Helper()
	Fatalf(format string, args ...any)
}
