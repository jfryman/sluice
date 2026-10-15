package maildir

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrStatePending: the folder's mbsync state has an uncompacted journal
// (an interrupted or running sync), so its UID pairs can't be trusted.
var ErrStatePending = errors.New("mbsync state has a pending journal")

// FarUIDs reads a folder's .mbsyncstate: the far (server) UIDVALIDITY and
// a map from near UID (the U= in local file names) to far UID. Read it
// under mbsync.lock. See lode/mail/server-moves.md.
//
//	FarUidValidity 1467142788
//	NearUidValidity 1788208369
//	...
//	<blank>
//	5 4 S        far UID 5 <-> near U=4
func FarUIDs(root, folder string) (validity uint32, nearToFar map[uint32]uint32, err error) {
	dir := filepath.Join(root, folder)
	if _, err := os.Stat(filepath.Join(dir, ".mbsyncstate.journal")); err == nil {
		return 0, nil, fmt.Errorf("%s: %w", folder, ErrStatePending)
	}
	f, err := os.Open(filepath.Join(dir, ".mbsyncstate"))
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	nearToFar = map[uint32]uint32{}
	body := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !body {
			if strings.TrimSpace(line) == "" {
				body = true
				continue
			}
			if k, v, ok := strings.Cut(line, " "); ok && k == "FarUidValidity" {
				n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
				if err != nil {
					return 0, nil, fmt.Errorf("%s: FarUidValidity %q: %w", folder, v, err)
				}
				validity = uint32(n)
			}
			continue
		}
		fs := strings.Fields(line)
		if len(fs) < 2 {
			continue
		}
		// Unpaired entries (0 or negative on one side) aren't on both ends yet.
		far, err1 := strconv.ParseInt(fs[0], 10, 64)
		near, err2 := strconv.ParseInt(fs[1], 10, 64)
		if err1 != nil || err2 != nil || far <= 0 || near <= 0 || far > 1<<32-1 || near > 1<<32-1 {
			continue
		}
		nearToFar[uint32(near)] = uint32(far)
	}
	if err := sc.Err(); err != nil {
		return 0, nil, err
	}
	if validity == 0 {
		return 0, nil, fmt.Errorf("%s: .mbsyncstate has no FarUidValidity", folder)
	}
	return validity, nearToFar, nil
}

// NearUID is the U= of a maildir file name (0 if it has none).
func NearUID(base string) uint32 {
	n, err := strconv.ParseUint(ParseName(base).UID, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}
