# remotestore — sing-box node-side RemoteStore adapter

`RemoteStore` is a `sessionadmission.SessionStore` implementation backed by the
central **X-NET Session Manager** over gRPC. It drops into the admission `Gate`
with no change to the Gate or the router (Property 7 / interface parity).

## Strict gRPC/proto isolation

All gRPC and protobuf imports are confined to THIS subpackage (`remotestore` and
its generated `managerpb`). The parent `sessionadmission` package — which holds
the `SessionStore` interface and the `Gate` — MUST NOT import any grpc/proto
package. This keeps the interface seam clean so a non-remote build never pulls in
gRPC. The isolation is asserted by an import-audit test
(`import_isolation_test.go`).

## Proto contract & code generation

`proto/session_manager.proto` is **byte-identical** to the manager's canonical
copy at `f:\xnet\session-manager\proto\sessionmanagerv1\session_manager.proto`
for every package name, message, field, field number, and service/RPC — so the
two separate Go modules stay **wire-compatible**.

The ONLY intentional divergence is the `option go_package` line:

```
// fork copy:
option go_package = "github.com/sagernet/sing-box/experimental/sessionadmission/remotestore/managerpb;managerpb";
// manager copy:
option go_package = "github.com/xpanel-cp/xnet-session-manager/gen/sessionmanagerv1;sessionmanagerv1";
```

A different Go import path is **required** because the fork is a separate Go
module (`github.com/sagernet/sing-box`) and cannot import the manager module's
path. The Go import path is a build-time concern only; it does **NOT** affect the
protobuf wire format, so the two sides remain fully interoperable.

### Regenerate the stubs

Toolchain (installed in `GOPATH/bin`): `buf` 1.72 + `protoc-gen-go` v1.36.11 +
`protoc-gen-go-grpc` v1.6.2. `buf` compiles the `.proto` internally — no `protoc`
C++ binary is needed.

From this directory (`experimental/sessionadmission/remotestore`), in Windows
PowerShell (prepend `GOPATH/bin` to `PATH` in the same command):

```powershell
$env:PATH += ";$(go env GOPATH)\bin"; buf generate
```

Output lands in `./managerpb` (`buf.gen.yaml` mirrors the manager's, differing
only in the output dir). Commit the generated `managerpb/*.pb.go`.

When the contract changes, regenerate **both** the manager and this fork copy
from the same definitions so the wire contract stays locked on both sides.
