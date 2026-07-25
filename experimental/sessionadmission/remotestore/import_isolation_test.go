package remotestore

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenImports are import paths the parent sessionadmission package (the
// SessionStore interface + the Gate) MUST NOT depend on. gRPC/proto are confined
// to THIS remotestore subpackage; the interface seam stays transport-free so a
// build that never wires a RemoteStore pulls in no gRPC.
var forbiddenImports = []string{
	"google.golang.org/grpc",
	"google.golang.org/protobuf",
	"remotestore/managerpb",
}

// TestSessionAdmissionPackageHasNoGRPCImports is the structural import audit for
// Property 7's isolation clause (Task 11.1 acceptance note). It parses every Go
// file that belongs to the parent sessionadmission package (the directory one
// level up, non-recursively — the remotestore subpackage is a separate package
// and is excluded) and fails if any imports a gRPC/proto path.
func TestSessionAdmissionPackageHasNoGRPCImports(t *testing.T) {
	parentDir := ".." // experimental/sessionadmission

	entries, err := os.ReadDir(parentDir)
	if err != nil {
		t.Fatalf("read sessionadmission dir: %v", err)
	}

	fset := token.NewFileSet()
	scanned := 0
	for _, entry := range entries {
		// Only the sessionadmission package's OWN .go files. Skip subdirectories
		// (including this remotestore package) so we audit exactly the interface
		// + Gate package.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join(parentDir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scanned++
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			for _, bad := range forbiddenImports {
				if strings.Contains(p, bad) {
					t.Errorf("%s imports forbidden package %q (gRPC/proto must stay confined to the remotestore subpackage)", path, p)
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no sessionadmission package files were scanned; audit is ineffective")
	}
	t.Logf("import audit passed: scanned %d sessionadmission package files, none import gRPC/proto", scanned)
}
