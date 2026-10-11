package runtimefiles

import (
	"fmt"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func translateID(id int, mappings []IDMap, toHost bool) (int, error) {
	if len(mappings) == 0 {
		return id, nil
	}
	for _, mapping := range mappings {
		from, to := mapping.Host, mapping.Namespace
		if toHost {
			from, to = mapping.Namespace, mapping.Host
		}
		if int64(id) >= from && int64(id)-from < mapping.Size {
			return int(to + int64(id) - from), nil
		}
	}
	return 0, fmt.Errorf("%w: UID/GID is outside the backend user-namespace map; use an administrator-managed transfer", datafiles.ErrUnsupported)
}

func (d Descriptor) namespaceOwner(uid, gid int) (int, int, error) {
	owner, err := translateID(uid, d.UIDMap, false)
	if err != nil {
		return 0, 0, err
	}
	group, err := translateID(gid, d.GIDMap, false)
	return owner, group, err
}

func (d Descriptor) hostOwner(uid, gid int) (int, int, error) {
	owner, err := translateID(uid, d.UIDMap, true)
	if err != nil {
		return 0, 0, err
	}
	group, err := translateID(gid, d.GIDMap, true)
	return owner, group, err
}
