// Package pysdk embeds the MLDojo Python SDK and the wandb shim so the agent
// can drop them on PYTHONPATH for every run.
package pysdk

import "embed"

//go:embed mldojo/*.py wandb_shim/wandb/*.py
var FS embed.FS
