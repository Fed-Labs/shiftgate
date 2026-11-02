package main

import (
	"fmt"
	"io"
	"strings"
)

// commandNames lists every top-level command in one place: the dispatcher,
// the usage text, the completion generator, and the "unknown command" error
// all read from here so they cannot drift apart. Aliases come last.
var commandNames = []string{
	"help", "doctor", "machines", "workload", "checkpoint", "restore", "fork",
	"clone", "connect", "connections", "disconnect", "migrate", "failover",
	"standby", "status", "update", "logs", "gc", "completion", "version",
	"login", "logout", "whoami", "plans", "storage", "fleet", "marketplace",
	"trial", "refer", "market", "workloads", "referral",
}

// commandUsage holds the per-command help block `shiftgate help COMMAND`
// (and `shiftgate COMMAND --help`) prints.
var commandUsage = map[string]string{
	"doctor": `Usage: shiftgate doctor

Check local migration prerequisites: CRIU, the kernel, the state directory.
Prints one line per check and exits 3 when a required check fails — the same
exit code under --json.`,
	"machines": `Usage: shiftgate machines [list]
       shiftgate machines register --machine-id ID --name NAME --agent-url URL
       shiftgate machines capability NAME [--kind number|text|boolean] [--minimum N]

Without arguments, shows this machine (id, OS, CPU, memory, GPU, CRIU).
'register' and 'capability' always talk to the control plane (the platform
endpoint by default; override with --control-url), so they need
'shiftgate login' first. With --control-url set, 'machines' shows the fleet.`,
	"workloads": `Usage: shiftgate workloads

Shorthand for 'shiftgate workload list' (fleet list when --control-url is set).`,
	"workload": `Usage: shiftgate workload SUBCOMMAND [args]

Subcommands:
  create NAME --path PATH [--workdir DIR] [--start] [--network preserve|reconnect|drain]
           [--device-policy reject_incompatible|warn_incompatible] [--cpu N] [--memory SIZE]
           [--env KEY=VALUE]...] [--gpu vendor=...] [--checkpoint-every DUR [--checkpoint-keep N]]
           -- COMMAND [ARGS...]
  list
  inspect ID
  policy NAME (--every DURATION [--keep N] | --off)
  failover NAME (--to URL [--machine-id ID] [--keep N] | --off)
  start | pause | resume ID
  stop ID [--timeout DURATION]
  delete ID [--yes]
  logs ID [--tail N]

Timeouts accept plain seconds (60) or durations (60s, 15m). 'delete' prompts
on a terminal and requires --yes in scripts.`,
	"checkpoint": `Usage: shiftgate checkpoint create WORKLOAD [--parent ID] [--leave-running|--stop-when-done]
           [--tcp-state] [--timeout DURATION]
       shiftgate checkpoint list [--workload ID|NAME]
       shiftgate checkpoint inspect ID
       shiftgate checkpoint mirror ID

'list' columns: KIND is full or incremental (incrementals chain off a parent);
SIZE is the pre-deduplication size, STORED is what the content-addressed store
kept after deduplication. --workload filters by workload id or by the name
shown in the WORKLOAD column.`,
	"restore": `Usage: shiftgate restore CHECKPOINT [--lazy] [--timeout DURATION]

Restores a checkpoint locally. --lazy starts the process before its memory is
fully loaded and streams pages in on demand (needs userfaultfd).`,
	"fork": `Usage: shiftgate fork WORKLOAD [--name NAME] [--root PATH] [--checkpoint ID]
           [--activate] [--timeout DURATION]
       shiftgate fork list
       shiftgate fork inspect FORK

Derives an independent workload (own root, key namespace, checkpoint) from a
workload or one of its checkpoints.`,
	"clone": `Usage: shiftgate clone CHECKPOINT [--count N] [--prefix NAME] [--parallel N]
           [--timeout DURATION] [--wait-ready] [--wait-timeout DURATION]
       shiftgate clone list
       shiftgate clone inspect CLONE
       shiftgate clone rollback CLONE

Derives many independent running workloads from one checkpoint. --wait-ready
blocks until every spawned workload passes its health check (or is running)
and prints per-worker ready latency; --wait-timeout bounds the wait.
'rollback' tears down an interrupted set and prompts before doing so.`,
	"connect": `Usage: shiftgate connect ALIAS --to https://HOST:PORT --machine-id ID [--server-name NAME]

Saves a remote machine alias for migrate/failover destinations.`,
	"connections": `Usage: shiftgate connections

Lists saved remote machine aliases.`,
	"disconnect": `Usage: shiftgate disconnect ALIAS

Removes a saved remote machine alias.`,
	"migrate": `Usage: shiftgate migrate WORKLOAD --to ALIAS|https://HOST:PORT [--machine-id ID]
           [--server-name NAME] [--mode cold|live] [--pre-copy-passes N]
           [--dry-run|--preflight] [--wait|--wait=false] [--timeout DURATION]

Moves a workload to another machine. --dry-run (alias --preflight) reports
whether the migration would be admitted without moving anything.`,
	"failover": `Usage: shiftgate failover [status]

Shows what this machine replicates to warm standbys.`,
	"standby": `Usage: shiftgate standby list
       shiftgate standby trigger WORKLOAD_ID [--checkpoint ID] [--lazy] [--timeout DURATION]

'list' shows the warm-standby duties this machine holds; 'trigger' restores one
now, with the command itself as the confirmation.`,
	"status": `Usage: shiftgate status [MIGRATION]

Shows migration status: all migrations, or one with its event stream.`,
	"update": `Usage: shiftgate update status | check | apply [--version V] | rollback [--backup ID] [--yes]
       shiftgate update block VERSION [--reason TEXT]
       shiftgate update unblock VERSION

Manages this machine's agent updates through the local API. 'rollback'
reinstalls a preserved binary and prompts before doing so.`,
	"logs": `Usage: shiftgate logs WORKLOAD [--tail N]

Shows workload logs. The output is the raw log text, including under --json.`,
	"gc": `Usage: shiftgate gc [--grace DURATION] [--dry-run]

Runs chunk garbage collection against the running agent, online: as root it
plans unreferenced chunks (--dry-run, the default) and deletes them with
--dry-run=false, under the collector's two-phase grace period. Requires root
(sudo shiftgate gc); the offline 'shift-gc' sweep is the fallback when the
agent cannot take a live sweep — the command prints its exact invocation. The
plan is always printed as JSON, with or without --json.`,
	"completion": `Usage: shiftgate completion bash|zsh|fish

Generates a shell completion snippet.`,
	"version": `Usage: shiftgate version

Prints the SHIFT version.`,
	"help": `Usage: shiftgate help [COMMAND]

Prints the global usage or one command's usage block.`,
	"login": `Usage: shiftgate login [--email ADDRESS] [--sso]

Logs in to the control plane (the platform endpoint by default) and stores
the session. The password is prompted, never a flag.`,
	"logout": `Usage: shiftgate logout

Revokes the stored session.`,
	"whoami": `Usage: shiftgate whoami

Shows the logged-in identity and organizations.`,
	"plans": `Usage: shiftgate plans

Shows the public plan catalog.`,
	"trial": `Usage: shiftgate trial start|status [--org ID]

The 14-day Business trial, once per organization, no card. 'start' begins it;
'status' shows the days left and whether it converted.`,
	"refer": `Usage: shiftgate refer [--org ID]

Prints your referral code and share link: when a team you invite runs its
first move, you both get Pro free for a month (up to 12). 'referral' is an
alias.`,
	"storage": `Usage: shiftgate storage [--org ID]

Shows hosted checkpoint storage status and agent setup lines.`,
	"fleet": `Usage: shiftgate fleet [--org ID] SUBCOMMAND [args]

Subcommands:
  machines [list|register|capability]
  workloads | migrations [ID] | checkpoints [WORKLOAD]
  audit
  api-keys [create --name N [--scopes S] | revoke KEY --yes]
  retention [--audit-days N] [--checkpoint-days N] [--deleted-storage-days N]
  sso [--enforce DOMAIN | --disable]
  entitlement
  usage [--from YYYY-MM-DD] [--to YYYY-MM-DD]`,
	"marketplace": `Usage: shiftgate marketplace [--org ID] SUBCOMMAND [args]
       shiftgate market ... (alias)

Subcommands:
  offers | inventory
  publish --machine ID --cpu N --memory SIZE [--region R] [--country C] [--visibility V] ...
  withdraw --offer ID --yes
  place --workload ID --cpu N --memory SIZE [--checkpoint ID] ...
  reserve --offer ID --workload ID --cpu N --memory SIZE ...
  reservations [--offer ID]
  commit | release | fail --reservation ID [--reason TEXT]`,
}

func printUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, `Usage: shiftgate [--agent ENDPOINT] [--control-url URL] [--token-store FILE]
           [--json] [--timeout DURATION] COMMAND

Global flags may appear before or after the command (never after '--'):
  --agent ENDPOINT    agent endpoint (default unix:///run/shift/agent.sock,
                      or $SHIFT_AGENT_ENDPOINT)
  --control-url URL   control-plane URL (empty means the platform endpoint,
                      or $SHIFT_CONTROL_URL)
  --token-store FILE  where the control-plane session is stored
  --json              emit JSON
  --timeout DURATION  request timeout; after a command, that command's
                      operation timeout (accepts 30 or 30s / 15m)

Agent commands (local machine):
  doctor                         Check local migration prerequisites
  machines                       Show this machine (register|capability via
                                  the control plane)
  workloads                      Shorthand for workload list
  workload create|list|inspect|policy|failover|start|pause|resume|stop|delete|logs
                                  Manage workloads
  checkpoint create|list|inspect|mirror
                                  Manage checkpoints
  restore CHECKPOINT             Restore a checkpoint locally
  fork WORKLOAD|list|inspect     Derive an independent workload from a state
  clone CHECKPOINT|list|inspect|rollback
                                  Derive many independent running workloads
                                  from one checkpoint at once
  connect ALIAS --to URL --machine-id ID
                                  Save a secure remote machine alias
  connections                    List saved remote machines
  disconnect ALIAS               Remove a saved remote alias
  migrate WORKLOAD --to ALIAS|URL Move a workload to another machine
  failover [status]              Show what this machine replicates to standbys
  standby list|trigger           Manage warm-standby duties and fail over
  status [MIGRATION]             Show migration status
  update status|check|apply|rollback|block|unblock
                                  Manage this machine's updates
  logs WORKLOAD                  Show workload logs
  gc                             Collect unreferenced chunks online (root; offline 'shift-gc' fallback)
  completion bash|zsh|fish       Generate shell completion
  version                        Print version

Control-plane commands (shiftgate login first; --control-url or SHIFT_CONTROL_URL, empty meaning the platform endpoint):
  login [--sso]                  Log in and store a session (single sign-on with --sso)
  logout                         Revoke the session and clear the stored tokens
  whoami                         Show identity and organizations
  plans                          Show the public plan catalog
  storage [--org ID]             Show hosted checkpoint storage status and agent setup
  fleet machines|workloads|migrations|checkpoints|audit|retention|sso|api-keys|entitlement|usage
                                  Manage the organization's registry
  marketplace offers|inventory|place|publish|withdraw|reservations|reserve|commit|release|fail
                                  Trade compute on the marketplace
  trial start|status [--org ID]   Start or inspect the 14-day Business trial
  refer [--org ID]                Show your referral code and share link

Run 'shiftgate help COMMAND' for one command's usage. With --control-url set,
'machines' and 'workloads' show the fleet view. 'inspect' subcommands always
emit JSON.`)
}

// printCommandHelp prints the usage block for one command, or the global
// usage when no topic is given.
func printCommandHelp(writer io.Writer, topic string) error {
	if topic == "" {
		printUsage(writer)
		return nil
	}
	if topic == "market" {
		topic = "marketplace"
	}
	if topic == "machine" {
		topic = "machines"
	}
	if topic == "referral" {
		topic = "refer"
	}
	usage, ok := commandUsage[topic]
	if !ok {
		return usageError(fmt.Sprintf("no help topic %q; known commands: %s", topic, strings.Join(commandNames, " ")))
	}
	_, _ = fmt.Fprintln(writer, usage)
	return nil
}

// suggestCommand names the closest known command to a typo, if any is close
// enough to be worth suggesting.
func suggestCommand(argument string) string {
	best, bestDistance := "", 3
	lowered := strings.ToLower(argument)
	for _, candidate := range commandNames {
		distance := editDistance(lowered, candidate)
		if distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	if best == "" {
		return ""
	}
	return "; did you mean 'shiftgate " + best + "'?"
}

// editDistance is the classic Levenshtein distance, enough to catch the
// typos operators actually make.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min3(current[j-1]+1, previous[j]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// unknownSubcommandError lists the valid subcommands next to a typo.
func unknownSubcommandError(command, subcommand string, valid []string) error {
	return usageError(fmt.Sprintf("unknown %s subcommand %q; valid: %s", command, subcommand, strings.Join(valid, ", ")))
}

func completion(writer io.Writer, arguments []string) error {
	if len(arguments) != 1 {
		return usageError("usage: shiftgate completion bash|zsh|fish")
	}
	commands := strings.Join(commandNames, " ")
	switch arguments[0] {
	case "bash":
		_, _ = fmt.Fprintf(writer, "complete -W %q shiftgate\n", commands)
	case "zsh":
		_, _ = fmt.Fprintf(writer, "#compdef shiftgate\n_arguments '1:command:(%s)'\n", commands)
	case "fish":
		for _, command := range commandNames {
			_, _ = fmt.Fprintf(writer, "complete -c shiftgate -f -n '__fish_use_subcommand' -a %s\n", command)
		}
	default:
		return usageError("unsupported shell " + arguments[0])
	}
	return nil
}
