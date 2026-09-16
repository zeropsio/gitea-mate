// Package giteamate exists for one reason: the files under `import/` are
// contracts the Mate app and the broker share, and the broker needs one of them
// at run time. Go's embed only reaches downwards, so the embed lives at the
// root and the file keeps its single home.
package giteamate

import _ "embed"

// RunnerImport is `import/runner.yaml`, the services-only import the broker
// sends when a group's first workflow appears. Its two placeholders —
// __HOSTNAME__ and __TOKEN__ — are filled in by internal/pipeline.
//
//go:embed import/runner.yaml
var RunnerImport string
