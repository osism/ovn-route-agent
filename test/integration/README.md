# Integration tests

The integration test harness is documented at
**<https://osism.github.io/ovn-network-agent/contributing/integration-tests>**.

The Markdown source lives at
[`docs/contributing/integration-tests.md`](../../docs/contributing/integration-tests.md);
edit that file when adding new scenarios or changing local-run instructions.

Quick reference:

```sh
sudo ./test/integration/setup.sh    # one-time, installs packages and starts services
make test-integration                # builds the binary and runs the tagged tests
```
