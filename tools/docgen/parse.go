package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
)

// sourceInfo bundles every fact the renderer needs about the upstream
// Go source. The parser is responsible for fully resolving references
// across config.go and metrics.go so the renderers can stay pure
// string formatters.
type sourceInfo struct {
	// Namespace is the Prometheus metric namespace prefix.
	Namespace string

	// Structs holds the parsed struct declarations keyed by Go type
	// name (Config, PortForwardRule, PortForwardVIP, configFile).
	Structs map[string]*structInfo

	// Flags are CLI flags declared in loadConfig, in source order.
	Flags []flagInfo

	// FlagByField indexes Flags by their associated Config field.
	FlagByField map[string]*flagInfo

	// EnvByField maps Config field name to the env var that sets it.
	EnvByField map[string]string

	// YAMLByField maps Config field name to the YAML key (resolved
	// through configFile + applyFileConfig).
	YAMLByField map[string]string

	// DefaultByField maps Config field name to the literal default
	// taken from the `cfg := Config{...}` composite in loadConfig.
	DefaultByField map[string]string

	// Metrics are Prometheus collectors declared in newMetricsRegistry,
	// in source order.
	Metrics []metricInfo
}

type structInfo struct {
	Name   string
	Fields []structField
}

type structField struct {
	Name    string
	Type    string
	YAMLTag string
	Comment string
}

type flagInfo struct {
	Name        string // CLI flag name (without leading --)
	Kind        string // "string" | "int" | "bool"
	Default     string // raw literal text (e.g. `"br-ex"`, `60`, `false`)
	Usage       string
	ConfigField string // Go field on Config that this flag writes to

	// ImplicitEnv is the env var pulled from an `os.Getenv("…")`
	// expression used as the flag's default (e.g. `--config`
	// defaults to `os.Getenv("OVN_NETWORK_CONFIG")`). Such flags
	// have no entry in applyEnvConfig but should still be cross-
	// referenced as env-var configurable.
	ImplicitEnv string
}

type metricInfo struct {
	Name        string   // unqualified metric name (Opts.Name)
	FullName    string   // namespace + "_" + name
	Kind        string   // "counter" | "gauge" | "histogram"
	IsVec       bool     // true for *Vec collectors
	Labels      []string // label names (nil for non-Vec)
	Help        string
	StructField string // metricsRegistry field that holds this collector
	// LabelValues maps each declared label to the literal values
	// seen in the bootstrap `WithLabelValues(...)` calls inside
	// newMetricsRegistry, in first-seen order. Empty for metrics
	// that are never pre-populated.
	LabelValues map[string][]string
}

func parseSource(root string) (*sourceInfo, error) {
	fset := token.NewFileSet()
	cfgFile, err := parser.ParseFile(fset, filepath.Join(root, "config.go"), nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse config.go: %w", err)
	}
	metricsFile, err := parser.ParseFile(fset, filepath.Join(root, "metrics.go"), nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse metrics.go: %w", err)
	}

	info := &sourceInfo{
		Structs:        map[string]*structInfo{},
		FlagByField:    map[string]*flagInfo{},
		EnvByField:     map[string]string{},
		YAMLByField:    map[string]string{},
		DefaultByField: map[string]string{},
	}

	parseStructs(cfgFile, info)
	parseLoadConfig(cfgFile, info)
	parseApplyFileConfig(cfgFile, info)
	parseApplyEnvConfig(cfgFile, info)

	if err := parseMetrics(metricsFile, info); err != nil {
		return nil, err
	}

	return info, nil
}

// parseStructs collects every top-level struct declaration with its
// field-level documentation. We deliberately preserve fields verbatim
// (Go type as written in source, including pointer/slice prefixes)
// because that text is what we surface in the reference.
func parseStructs(f *ast.File, info *sourceInfo) {
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				continue
			}
			si := &structInfo{Name: ts.Name.Name}
			for _, field := range st.Fields.List {
				for _, name := range field.Names {
					sf := structField{
						Name: name.Name,
						Type: exprString(field.Type),
					}
					if field.Tag != nil {
						sf.YAMLTag = structTagValue(field.Tag.Value, "yaml")
					}
					sf.Comment = fieldComment(field)
					si.Fields = append(si.Fields, sf)
				}
			}
			info.Structs[ts.Name.Name] = si
		}
	}
}

// parseLoadConfig walks loadConfig to extract three independent
// facts: every flag declared on the FlagSet, the literal default
// values from `cfg := Config{...}`, and the flag→Config-field
// mapping encoded in the `fs.Visit` switch.
func parseLoadConfig(f *ast.File, info *sourceInfo) {
	fn := findFunc(f, "loadConfig")
	if fn == nil || fn.Body == nil {
		return
	}

	// Map local flag-variable name (e.g. fOVNSB) to the flag info
	// extracted from its fs.<Type>("name", default, "usage") call.
	flagsByVar := map[string]*flagInfo{}

	for _, stmt := range fn.Body.List {
		switch s := stmt.(type) {
		case *ast.DeclStmt:
			gen, ok := s.Decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					call, ok := vs.Values[i].(*ast.CallExpr)
					if !ok {
						continue
					}
					fi := flagFromCall(call)
					if fi == nil {
						continue
					}
					flagsByVar[name.Name] = fi
				}
			}
		case *ast.AssignStmt:
			collectDefaults(s, info)
		case *ast.ExprStmt:
			// fs.Visit(func(f *flag.Flag) { switch f.Name { case ... } })
			collectFlagFieldMapping(s, flagsByVar)
		}
	}

	// Now flagsByVar contains both the metadata from fs.<Type>(...) and,
	// after collectFlagFieldMapping, the ConfigField target. Preserve
	// source order by re-walking the var declarations.
	for _, stmt := range fn.Body.List {
		ds, ok := stmt.(*ast.DeclStmt)
		if !ok {
			continue
		}
		gen, ok := ds.Decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				fi, ok := flagsByVar[name.Name]
				if !ok || fi.Name == "" {
					continue
				}
				info.Flags = append(info.Flags, *fi)
				if fi.ConfigField != "" {
					last := &info.Flags[len(info.Flags)-1]
					info.FlagByField[fi.ConfigField] = last
				}
			}
		}
	}
}

// flagFromCall recognises calls of the form
// `fs.<Kind>("name", default, "usage")` and returns a partially
// populated flagInfo (ConfigField is filled in later).
func flagFromCall(call *ast.CallExpr) *flagInfo {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok || recv.Name != "fs" {
		return nil
	}
	var kind string
	switch sel.Sel.Name {
	case "String":
		kind = "string"
	case "Int":
		kind = "int"
	case "Bool":
		kind = "bool"
	default:
		return nil
	}
	if len(call.Args) < 3 {
		return nil
	}
	name, ok := stringLit(call.Args[0])
	if !ok {
		return nil
	}
	usage, ok := stringLit(call.Args[2])
	if !ok {
		return nil
	}
	fi := &flagInfo{
		Name:    name,
		Kind:    kind,
		Default: exprString(call.Args[1]),
		Usage:   usage,
	}
	if env, ok := getenvCallArg(call.Args[1]); ok {
		fi.ImplicitEnv = env
		fi.Default = ""
	}
	return fi
}

// getenvCallArg returns the literal argument of an `os.Getenv("X")`
// call expression, or ("", false) if expr is not such a call.
func getenvCallArg(expr ast.Expr) (string, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Getenv" {
		return "", false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "os" {
		return "", false
	}
	if len(call.Args) != 1 {
		return "", false
	}
	return stringLit(call.Args[0])
}

// collectDefaults extracts default values from `cfg := Config{ ... }`.
// Only key-value composite-literal entries are considered.
func collectDefaults(s *ast.AssignStmt, info *sourceInfo) {
	if s.Tok != token.DEFINE || len(s.Lhs) != 1 || len(s.Rhs) != 1 {
		return
	}
	ident, ok := s.Lhs[0].(*ast.Ident)
	if !ok || ident.Name != "cfg" {
		return
	}
	cl, ok := s.Rhs[0].(*ast.CompositeLit)
	if !ok {
		return
	}
	typIdent, ok := cl.Type.(*ast.Ident)
	if !ok || typIdent.Name != "Config" {
		return
	}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		info.DefaultByField[key.Name] = exprString(kv.Value)
	}
}

// collectFlagFieldMapping walks `fs.Visit(func(f *flag.Flag) { switch f.Name { ... } })`
// and records the flag-name → Config-field mapping encoded in each
// `case "flag-name":` clause body.
func collectFlagFieldMapping(es *ast.ExprStmt, flagsByVar map[string]*flagInfo) {
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Visit" {
		return
	}
	if len(call.Args) != 1 {
		return
	}
	fn, ok := call.Args[0].(*ast.FuncLit)
	if !ok || fn.Body == nil {
		return
	}

	// Build local-var → flag-name index so we can match the body of
	// each case to the right flag entry.
	flagByLocalVar := map[string]*flagInfo{}
	for v, fi := range flagsByVar {
		flagByLocalVar[v] = fi
	}

	for _, stmt := range fn.Body.List {
		sw, ok := stmt.(*ast.SwitchStmt)
		if !ok {
			continue
		}
		for _, c := range sw.Body.List {
			cc, ok := c.(*ast.CaseClause)
			if !ok || len(cc.List) != 1 {
				continue
			}
			flagName, ok := stringLit(cc.List[0])
			if !ok {
				continue
			}
			field := extractConfigField(cc.Body)
			if field == "" {
				continue
			}
			for _, fi := range flagByLocalVar {
				if fi.Name == flagName {
					fi.ConfigField = field
				}
			}
		}
	}
}

// extractConfigField returns the Config field name written to by the
// first `cfg.<Field> = ...` assignment encountered in a case body. We
// look only at the first assignment because some cases wrap a
// time.ParseDuration call in an `if`, but the first cfg.X access
// inside still identifies the field.
func extractConfigField(body []ast.Stmt) string {
	var found string
	for _, st := range body {
		ast.Inspect(st, func(n ast.Node) bool {
			if found != "" {
				return false
			}
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 {
				return true
			}
			sel, ok := as.Lhs[0].(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "cfg" {
				return true
			}
			found = sel.Sel.Name
			return false
		})
		if found != "" {
			break
		}
	}
	return found
}

// parseApplyFileConfig walks applyFileConfig to discover the
// configFile.Y field that backs each Config.X assignment. The
// resulting Config-field → YAML-key mapping lets us print the
// canonical YAML key alongside every flag. Each top-level statement
// in applyFileConfig is treated as a single (fc-field, cfg-field)
// pair so we correctly resolve assignments wrapped in `if`/parse
// blocks (e.g. `if fc.ReconcileInterval != "" { … cfg.ReconcileInterval = d }`).
func parseApplyFileConfig(f *ast.File, info *sourceInfo) {
	fn := findFunc(f, "applyFileConfig")
	if fn == nil || fn.Body == nil {
		return
	}

	cfgFileStruct := info.Structs["configFile"]
	if cfgFileStruct == nil {
		return
	}
	yamlByFileField := map[string]string{}
	for _, sf := range cfgFileStruct.Fields {
		if sf.YAMLTag != "" {
			yamlByFileField[sf.Name] = sf.YAMLTag
		}
	}

	for _, stmt := range fn.Body.List {
		fileField := firstFCField(stmt)
		cfgField := firstCFGAssignment(stmt)
		if fileField == "" || cfgField == "" {
			continue
		}
		if yaml, ok := yamlByFileField[fileField]; ok {
			info.YAMLByField[cfgField] = yaml
		}
	}
}

// firstCFGAssignment returns the Config field on the LHS of the first
// `cfg.<Field> = …` assignment inside node.
func firstCFGAssignment(node ast.Node) string {
	var out string
	ast.Inspect(node, func(n ast.Node) bool {
		if out != "" {
			return false
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 {
			return true
		}
		sel, ok := as.Lhs[0].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "cfg" {
			return true
		}
		out = sel.Sel.Name
		return false
	})
	return out
}

// firstFCField returns the first selector of the form fc.<Field>
// found anywhere inside node. We dig through StarExpr, CallExpr, and
// SelectorExpr wrappers (e.g. `*fc.RouteTableID`,
// `time.ParseDuration(fc.X)`).
func firstFCField(node ast.Node) string {
	var out string
	ast.Inspect(node, func(n ast.Node) bool {
		if out != "" {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "fc" {
			return true
		}
		out = sel.Sel.Name
		return false
	})
	return out
}

// parseApplyEnvConfig walks applyEnvConfig and pairs each
// os.Getenv("NAME") call with the Config field assigned inside the
// enclosing `if` body.
func parseApplyEnvConfig(f *ast.File, info *sourceInfo) {
	fn := findFunc(f, "applyEnvConfig")
	if fn == nil || fn.Body == nil {
		return
	}
	for _, stmt := range fn.Body.List {
		ifs, ok := stmt.(*ast.IfStmt)
		if !ok || ifs.Init == nil {
			continue
		}
		envName := getenvFromIfInit(ifs.Init)
		if envName == "" {
			continue
		}
		field := extractConfigField(ifs.Body.List)
		if field == "" {
			continue
		}
		info.EnvByField[field] = envName
	}
}

// getenvFromIfInit returns the literal argument of an `os.Getenv("X")`
// call when it appears in the init position of an `if` statement.
func getenvFromIfInit(init ast.Stmt) string {
	as, ok := init.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 {
		return ""
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Getenv" {
		return ""
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "os" {
		return ""
	}
	if len(call.Args) != 1 {
		return ""
	}
	name, ok := stringLit(call.Args[0])
	if !ok {
		return ""
	}
	return name
}

// parseMetrics extracts the Prometheus namespace constant and every
// `prometheus.New<Kind>{Vec}` constructor used to populate the
// metricsRegistry struct literal in newMetricsRegistry.
func parseMetrics(f *ast.File, info *sourceInfo) error {
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name != "metricsNamespace" || i >= len(vs.Values) {
					continue
				}
				if v, ok := stringLit(vs.Values[i]); ok {
					info.Namespace = v
				}
			}
		}
	}

	fn := findFunc(f, "newMetricsRegistry")
	if fn == nil || fn.Body == nil {
		return fmt.Errorf("metrics.go: function newMetricsRegistry not found")
	}
	var cl *ast.CompositeLit
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if cl != nil {
			return false
		}
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		ident, ok := lit.Type.(*ast.Ident)
		if !ok || ident.Name != "metricsRegistry" {
			return true
		}
		cl = lit
		return false
	})
	if cl == nil {
		return fmt.Errorf("metrics.go: metricsRegistry composite literal not found")
	}

	// Map struct field name -> index in info.Metrics so we can look
	// the metric back up when walking the WithLabelValues bootstrap
	// calls below.
	indexByField := map[string]int{}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		fieldName, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		mi, ok := metricFromCall(kv.Value, info.Namespace)
		if !ok {
			continue
		}
		mi.StructField = fieldName.Name
		info.Metrics = append(info.Metrics, mi)
		indexByField[fieldName.Name] = len(info.Metrics) - 1
	}

	collectLabelValues(fn, indexByField, info.Metrics)
	return nil
}

// collectLabelValues walks newMetricsRegistry for calls of the form
// `m.<field>.WithLabelValues("v1", "v2", …).<op>(…)` and records the
// literal values against the corresponding metric. Values are
// associated positionally with the metric's declared labels.
func collectLabelValues(fn *ast.FuncDecl, indexByField map[string]int, metrics []metricInfo) {
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		outer, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		outerSel, ok := outer.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		inner, ok := outerSel.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		innerSel, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok || innerSel.Sel.Name != "WithLabelValues" {
			return true
		}
		fieldSel, ok := innerSel.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := fieldSel.X.(*ast.Ident)
		if !ok || recv.Name != "m" {
			return true
		}
		idx, ok := indexByField[fieldSel.Sel.Name]
		if !ok {
			return true
		}
		labels := metrics[idx].Labels
		if len(labels) == 0 {
			return true
		}
		if metrics[idx].LabelValues == nil {
			metrics[idx].LabelValues = map[string][]string{}
		}
		for i, arg := range inner.Args {
			if i >= len(labels) {
				break
			}
			val, ok := stringLit(arg)
			if !ok {
				continue
			}
			label := labels[i]
			if !containsString(metrics[idx].LabelValues[label], val) {
				metrics[idx].LabelValues[label] = append(metrics[idx].LabelValues[label], val)
			}
		}
		return true
	})
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// metricFromCall recognises calls of the form
// `prometheus.New<Kind>(prometheus.<Kind>Opts{...}, []string{labels...})`
// and extracts the metric's Name, Help, kind, and label set.
func metricFromCall(expr ast.Expr, namespace string) (metricInfo, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return metricInfo{}, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return metricInfo{}, false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "prometheus" {
		return metricInfo{}, false
	}
	kind, isVec := classifyConstructor(sel.Sel.Name)
	if kind == "" {
		return metricInfo{}, false
	}
	if len(call.Args) == 0 {
		return metricInfo{}, false
	}
	optsLit, ok := call.Args[0].(*ast.CompositeLit)
	if !ok {
		return metricInfo{}, false
	}
	var (
		name string
		help string
	)
	for _, e := range optsLit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Name":
			if v, ok := stringLit(kv.Value); ok {
				name = v
			}
		case "Help":
			if v, ok := stringLit(kv.Value); ok {
				help = v
			}
		}
	}
	if name == "" {
		return metricInfo{}, false
	}
	mi := metricInfo{
		Name:     name,
		FullName: name,
		Kind:     kind,
		IsVec:    isVec,
		Help:     help,
	}
	if namespace != "" {
		mi.FullName = namespace + "_" + name
	}
	if isVec && len(call.Args) >= 2 {
		mi.Labels = stringSliceLit(call.Args[1])
	}
	return mi, true
}

func classifyConstructor(name string) (kind string, isVec bool) {
	switch name {
	case "NewCounter":
		return "counter", false
	case "NewCounterVec":
		return "counter", true
	case "NewGauge":
		return "gauge", false
	case "NewGaugeVec":
		return "gauge", true
	case "NewHistogram":
		return "histogram", false
	case "NewHistogramVec":
		return "histogram", true
	}
	return "", false
}

func stringSliceLit(expr ast.Expr) []string {
	cl, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range cl.Elts {
		if v, ok := stringLit(e); ok {
			out = append(out, v)
		}
	}
	return out
}

// findFunc returns the top-level function declaration with the given
// name, or nil if absent.
func findFunc(f *ast.File, name string) *ast.FuncDecl {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		if fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// stringLit returns the unquoted value of a basic string literal.
func stringLit(expr ast.Expr) (string, bool) {
	bl, ok := expr.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// structTagValue mirrors reflect.StructTag.Get without reaching for
// the reflect package. The input is the raw `…` source tag.
func structTagValue(raw, key string) string {
	unquoted, err := strconv.Unquote(raw)
	if err != nil {
		return ""
	}
	for len(unquoted) > 0 {
		i := 0
		for i < len(unquoted) && unquoted[i] == ' ' {
			i++
		}
		unquoted = unquoted[i:]
		if unquoted == "" {
			break
		}
		i = 0
		for i < len(unquoted) && unquoted[i] != ':' && unquoted[i] > ' ' && unquoted[i] != '"' {
			i++
		}
		if i == 0 || i+1 >= len(unquoted) || unquoted[i] != ':' || unquoted[i+1] != '"' {
			break
		}
		tagKey := unquoted[:i]
		unquoted = unquoted[i+1:]
		i = 1
		for i < len(unquoted) && unquoted[i] != '"' {
			if unquoted[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(unquoted) {
			break
		}
		qval := unquoted[:i+1]
		unquoted = unquoted[i+1:]
		if tagKey == key {
			val, err := strconv.Unquote(qval)
			if err != nil {
				return ""
			}
			// The yaml tag may carry comma-separated options
			// (e.g. `yaml:"foo,omitempty"`); only the key matters.
			if idx := strings.Index(val, ","); idx >= 0 {
				val = val[:idx]
			}
			return val
		}
	}
	return ""
}

// fieldComment returns the first available source documentation for
// a struct field, preferring the trailing line comment (which the
// existing structs use to annotate YAML semantics) over the doc
// comment block above the field.
func fieldComment(field *ast.Field) string {
	if c := commentText(field.Comment); c != "" {
		return c
	}
	return commentText(field.Doc)
}

func commentText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	var parts []string
	for _, c := range g.List {
		text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
		text = strings.TrimSpace(strings.TrimPrefix(text, "/*"))
		text = strings.TrimSpace(strings.TrimSuffix(text, "*/"))
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

// exprString renders an arbitrary AST expression back to a compact
// source-like string. We avoid go/printer to keep the output stable
// across Go versions and free of trailing whitespace.
func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.BasicLit:
		return e.Value
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	case *ast.ArrayType:
		if e.Len == nil {
			return "[]" + exprString(e.Elt)
		}
		return "[" + exprString(e.Len) + "]" + exprString(e.Elt)
	case *ast.MapType:
		return "map[" + exprString(e.Key) + "]" + exprString(e.Value)
	case *ast.BinaryExpr:
		return exprString(e.X) + " " + e.Op.String() + " " + exprString(e.Y)
	case *ast.CallExpr:
		args := make([]string, 0, len(e.Args))
		for _, a := range e.Args {
			args = append(args, exprString(a))
		}
		return exprString(e.Fun) + "(" + strings.Join(args, ", ") + ")"
	case *ast.UnaryExpr:
		return e.Op.String() + exprString(e.X)
	case *ast.ParenExpr:
		return "(" + exprString(e.X) + ")"
	case *ast.CompositeLit:
		return exprString(e.Type) + "{…}"
	case *ast.InterfaceType:
		return "interface{}"
	}
	return ""
}
