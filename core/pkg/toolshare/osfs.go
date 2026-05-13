package toolshare

import "github.com/vadimtrunov/watchkeepers/core/pkg/localpatch"

// OSFS is the production [FS] implementation, re-exported from
// localpatch so callers of this package never have to reach into
// the sibling package directly. Iter-1 m1 fix (reviewer B):
// previously [FS] was aliased but [OSFS] was not — asymmetric
// with `hostedexport.OSFS` and forced CLI wiring to type-cast.
// Mirror the M9.6.a re-export pattern.
type OSFS = localpatch.OSFS
