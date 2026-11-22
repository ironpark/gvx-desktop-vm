package vmhelper

// Owner: workstream A.

import (
	"encoding/json"
	"fmt"
	"os"
)

// Main runs the helper subcommand args[0] with the rest of args and
// returns the process's exit code.
func Main(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "vm helper: want serve, check or install-rosetta")
		return 2
	}
	switch args[0] {
	case CmdServe:
		if err := serve(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "vm helper:", err)
			return 1
		}
		return 0
	case CmdCheck, CmdInstallRosetta:
		res := check(args[0] == CmdInstallRosetta)
		_ = json.NewEncoder(os.Stdout).Encode(res)
		return 0
	}
	fmt.Fprintf(os.Stderr, "vm helper: unknown subcommand %q\n", args[0])
	return 2
}
