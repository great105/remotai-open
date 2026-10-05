package hermes

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestControlEnabledOwnersDiscovery(t *testing.T) {
	root := t.TempDir()
	for _, entry := range []struct{ name, content string }{{"1", `{"schema":1,"managed":true,"auto_start":true}`}, {"2", `{"schema":1,"managed":true,"auto_start":false}`}, {"3", `{"schema":1,"managed":false,"auto_start":true}`}, {"other", `{"schema":1,"managed":true,"auto_start":true}`}} {
		p := filepath.Join(root, entry.name)
		_ = os.MkdirAll(p, 0700)
		_ = os.WriteFile(filepath.Join(p, "manager.json"), []byte(entry.content), 0600)
	}
	discover, ok := any(&Manager{}).(interface{ EnabledOwners(string) ([]int64, error) })
	if !ok {
		t.Fatal("startup owner enumeration unavailable")
	}
	owners, err := discover.EnabledOwners(root)
	if err != nil || !reflect.DeepEqual(owners, []int64{1}) {
		t.Fatalf("unsafe discovery: %v %v", owners, err)
	}
}
