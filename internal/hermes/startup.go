package hermes

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// Enumerate only the finite local manager metadata; never open home/config,
// profiles, secrets or transcript files. This does not start or install Hermes.
func (*Manager) EnabledOwners(usersRoot string) ([]int64, error) {
	entries, err := os.ReadDir(usersRoot)
	if errors.Is(err, os.ErrNotExist) {
		return []int64{}, nil
	}
	if err != nil {
		return nil, err
	}
	owners := []int64{}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		uid, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil || uid <= 0 || strconv.FormatInt(uid, 10) != entry.Name() {
			continue
		}
		f, err := os.Open(filepath.Join(usersRoot, entry.Name(), "manager.json"))
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, 16<<10+1))
		f.Close()
		if err != nil || len(data) > 16<<10 {
			continue
		}
		state := diskState{AutoStart: true}
		if json.Unmarshal(data, &state) != nil || state.Schema != 1 || !state.Managed || !state.AutoStart {
			continue
		}
		owners = append(owners, uid)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })
	return owners, nil
}
