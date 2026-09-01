package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// runsDirName is where detached runs keep their state, relative to the remote
// working directory. Anchoring it under the working directory rather than a
// fixed /workspace path means a run launched with --dir /data keeps its log
// beside the code it ran on, and `aq logs --dir /data` finds it.
const runsDirName = ".aq/runs"

// detachedRunScript is the receiving half of `aq run --detach`.
//
// Two heredocs, both QUOTED, so neither the user's command nor this script is
// re-expanded by the remote shell on the way in — the command is written to a
// file byte-for-byte and only interpreted when run.sh executes it. run.sh
// locates its own directory instead of having one interpolated, which is what
// lets it be written through a quoted heredoc at all.
//
// `nohup` is what makes the run outlive the ssh session: sshd signals the
// session's process group on disconnect, and nohup makes the child ignore
// SIGHUP. Redirecting stdout/stderr to a file and stdin from /dev/null is not
// optional either — ssh will not close the connection while the remote command
// still holds the tty.
const detachedRunScript = `set -e
base=__WORKDIR__/` + runsDirName + `
mkdir -p "$base"
id=$(date -u +%Y%m%d-%H%M%S)
d="$base/$id"
n=0
while ! mkdir "$d" 2>/dev/null; do n=$((n+1)); d="$base/$id-$n"; done
cat > "$d/cmd" <<'AQ_CMD_EOF'
__COMMAND__
AQ_CMD_EOF
cat > "$d/run.sh" <<'AQ_RUN_EOF'
#!/bin/sh
d=$(cd "$(dirname "$0")" && pwd)
cd __WORKDIR__ || exit 1
sh "$d/cmd"
echo $? > "$d/status"
AQ_RUN_EOF
chmod +x "$d/run.sh"
nohup "$d/run.sh" > "$d/log" 2>&1 < /dev/null &
echo $! > "$d/pid"
printf '%s\n' "${d##*/}"
`

// buildDetachedRunScript fills the template for one detached run.
//
// __WORKDIR__ is replaced with a shell-quoted path: the replacement lands
// inside a quoted heredoc for run.sh, so the bytes are written literally and
// are only parsed as shell later, when run.sh executes — which is exactly when
// the quoting needs to be valid.
func buildDetachedRunScript(workdir string, command []string) string {
	s := strings.ReplaceAll(detachedRunScript, "__WORKDIR__", shellQuote(workdir))
	return strings.Replace(s, "__COMMAND__", strings.Join(command, " "), 1)
}

// launchDetached starts the command on the box and returns the run id the box
// minted. The id comes from the box's own clock so it always sorts correctly
// against its neighbours, whatever the laptop's clock says.
func launchDetached(alias, workdir string, command []string, runner func(args []string) ([]byte, error)) (string, error) {
	if runner == nil {
		runner = func(args []string) ([]byte, error) {
			return exec.Command("ssh", args...).Output()
		}
	}
	args := buildSSHArgs(alias, "", nil, []string{buildDetachedRunScript(workdir, command)})
	out, err := runner(args)
	if err != nil {
		return "", fmt.Errorf("could not start the detached run: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		// The box is supposed to echo the run id as the script's only stdout.
		// Nothing back means the run may or may not have started, and reporting
		// a success we cannot name would leave the user with no way to find it.
		return "", fmt.Errorf("the box did not report a run id: the run may not have started; check `aq logs %s --list`", alias)
	}
	return id, nil
}
