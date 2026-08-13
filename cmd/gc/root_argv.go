package main

import "strings"

// rootCommandOptions controls side effects performed while constructing the
// Cobra tree. invocationArgs is always the injected run(args) slice and never
// includes argv[0].
type rootCommandOptions struct {
	invocationArgs            []string
	discoverPackCommands      bool
	eagerPackCommandDiscovery bool
}

func rootCommandOptionsForArgs(args []string) rootCommandOptions {
	discoverPackCommands := !rootInvocationSkipsPackDiscovery(args)
	return rootCommandOptions{
		invocationArgs:            append([]string(nil), args...),
		discoverPackCommands:      discoverPackCommands,
		eagerPackCommandDiscovery: discoverPackCommands,
	}
}

// rootInvocationSkipsPackDiscovery reports whether args can only resolve to
// a config-independent path: a recognized command that never needs city
// config (rootCommandSkipsPackDiscovery), or a bare/help/version-only
// invocation that Cobra answers directly without running any command.
// Anything else fails closed (returns false, i.e. discovery still runs) —
// pack discovery only adds work, it never changes whether a real command
// resolves.
func rootInvocationSkipsPackDiscovery(args []string) bool {
	command, ok := firstRootCommand(args)
	if ok {
		return rootCommandSkipsPackDiscovery(command)
	}
	return isHelpOrVersionOnlyInvocation(args)
}

// rootCommandSkipsPackDiscovery identifies built-in commands that cannot
// resolve to a pack binding. Pack discovery only adds city-config and pack
// loading work; each command still performs its normal scope and config
// resolution when it runs.
func rootCommandSkipsPackDiscovery(command string) bool {
	switch command {
	case "metrics", "bd", "git-credential", "dolt-state", "dolt-config", "bd-store-bridge",
		"help", "version", "completion":
		return true
	default:
		return false
	}
}

// isHelpOrVersionOnlyInvocation reports whether every token before the first
// command word (following the same persistent-scope grammar as
// firstRootCommand) is --help/-h/--version, or whether args has no command
// word at all. Cobra answers both shapes itself — printing help or the
// version string — without ever running loadCityConfig, so pack discovery
// has nothing to contribute.
func isHelpOrVersionOnlyInvocation(args []string) bool {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--":
			return false
		case isRootPersistentValueFlag(arg):
			if index+1 >= len(args) {
				return false
			}
			index++
		case isRootPersistentValueAssignment(arg):
			continue
		case arg == "--help" || arg == "-h" || arg == "--version":
			continue
		case strings.HasPrefix(arg, "-"):
			return false
		default:
			return false
		}
	}
	return true
}

// firstRootCommand returns the first command word under the root's narrow
// persistent-scope grammar. Unknown flags fail closed because this pre-scan
// cannot know whether a later token is their value. A separate known value
// flag consumes exactly one following token, including "--", matching pflag.
func firstRootCommand(args []string) (string, bool) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--":
			return "", false
		case isRootPersistentValueFlag(arg):
			if index+1 >= len(args) {
				return "", false
			}
			index++
		case isRootPersistentValueAssignment(arg):
			continue
		case strings.HasPrefix(arg, "-"):
			return "", false
		default:
			return arg, true
		}
	}
	return "", false
}

func isRootPersistentValueFlag(arg string) bool {
	switch arg {
	case "--city", "--rig", "--context", "--city-url", "--city-name":
		return true
	default:
		return false
	}
}

func isRootPersistentValueAssignment(arg string) bool {
	name, _, hasValue := strings.Cut(arg, "=")
	return hasValue && isRootPersistentValueFlag(name)
}
