// mldojo is the MLDojo CLI: a thin client of the API with --json
// output and stable exit codes, usable by humans and AI agents alike.
package main

import (
	"os"

	"github.com/lovemoon-ai/mldojo/cli/internal/commands"
)

func main() { os.Exit(commands.Execute()) }
