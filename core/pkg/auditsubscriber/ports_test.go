package auditsubscriber

import (
	"github.com/vadimtrunov/watchkeepers/core/pkg/eventbus"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// Compile-time assertions: the production seams satisfy the
// per-call [Bus] / [Writer] interfaces this package consumes.
//
// Same one-way import-cycle-break pattern documented in
// `docs/LESSONS.md` (cron.LocalPublisher, keeperslog.LocalKeepClient,
// lifecycle.LocalKeepClient): the audit subscriber holds the seam,
// production code passes concrete types in.
var (
	_ Bus    = (*eventbus.Bus)(nil)
	_ Writer = (*keeperslog.Writer)(nil)
)
