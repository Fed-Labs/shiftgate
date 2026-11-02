package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// flagNew builds a subcommand flag set that never prints Go's raw usage: all
// errors surface through the CLI's own usage text instead.
func flagNew(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

// global flag names: recognized anywhere in the command line (before "--"),
// so `shiftgate doctor --json` and `shiftgate --json doctor` behave the same.
// --timeout is the one exception: before the command it sets the request
// timeout, after the command it belongs to the command's own operation
// timeout, so it is only extracted from the leading segment.
var globalFlagNames = []string{"agent", "control-url", "token-store", "json", "timeout"}

// parseDurationOrSeconds accepts both a Go duration ("30s", "15m") and a
// plain integer of seconds ("30"), so one timeout grammar works everywhere.
func parseDurationOrSeconds(value string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, errors.New("empty duration")
	}
	if parsed, err := time.ParseDuration(trimmed); err == nil {
		if parsed < 0 {
			return 0, fmt.Errorf("invalid duration %q: must not be negative", value)
		}
		return parsed, nil
	}
	if seconds, err := strconv.ParseFloat(trimmed, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds * float64(time.Second)), nil
	}
	return 0, fmt.Errorf("invalid duration %q: use 30 for seconds or 30s / 15m", value)
}

// durationValue is a flag.Value that parses both forms.
type durationValue time.Duration

func (value *durationValue) String() string {
	if value == nil {
		return "0s"
	}
	return time.Duration(*value).String()
}

func (value *durationValue) Set(raw string) error {
	parsed, err := parseDurationOrSeconds(raw)
	if err != nil {
		return err
	}
	*value = durationValue(parsed)
	return nil
}

// durationFlag registers a duration flag that accepts seconds or durations.
func durationFlag(flags *flag.FlagSet, name string, fallback time.Duration, usage string) *time.Duration {
	target := new(time.Duration)
	*target = fallback
	flags.Var((*durationValue)(target), name, usage)
	return target
}

// parseCommandFlags parses flags and positionals in any order. Everything
// after a bare "--" is returned as positionals verbatim, so a workload
// command line may carry its own flags. Unlike flag.FlagSet.Parse, a
// positional does not stop flag parsing: `checkpoint create --timeout 60
// demo` and `checkpoint create demo --timeout 60` mean the same thing.
func parseCommandFlags(flags *flag.FlagSet, arguments []string) ([]string, error) {
	var flagTokens, positionals []string
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			positionals = append(positionals, arguments[index+1:]...)
			break
		}
		if len(argument) < 2 || argument[0] != '-' {
			positionals = append(positionals, argument)
			continue
		}
		name := strings.TrimLeft(argument, "-")
		hasValue := false
		if before, _, found := strings.Cut(name, "="); found {
			name, hasValue = before, true
		}
		entry := flags.Lookup(name)
		if entry == nil {
			return nil, unknownFlagError(flags.Name(), argument, name)
		}
		flagTokens = append(flagTokens, argument)
		if hasValue {
			continue
		}
		if boolean, ok := entry.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			continue
		}
		if index+1 >= len(arguments) {
			return nil, fmt.Errorf("flag %s needs a value", argument)
		}
		index++
		flagTokens = append(flagTokens, arguments[index])
	}
	if err := flags.Parse(flagTokens); err != nil {
		return nil, err
	}
	return positionals, nil
}

// unknownFlagError explains a stray flag: a global flag in the wrong place
// gets its remedy spelled out, anything else points at the command's help.
func unknownFlagError(command, argument, name string) error {
	switch name {
	case "json", "agent", "control-url", "token-store":
		return fmt.Errorf("unknown flag %q for %s: the global --%s must appear before the command (shiftgate --%s ... %s ...)", argument, command, name, name, command)
	case "timeout":
		return fmt.Errorf("unknown flag %q for %s: --timeout before the command is the request timeout; after a command it sets that command's operation timeout and is only accepted by commands that run one (checkpoint create, restore, migrate, fork, clone, workload stop, standby trigger)", argument, command)
	}
	return fmt.Errorf("unknown flag %q for %s; see 'shiftgate help %s'", argument, command, command)
}

// extractGlobalFlags pulls the global flags out of arguments wherever they
// appear and returns the remaining command line. Scanning stops at "--" so
// workload command lines keep their own flags. A --timeout after the first
// command word is left in place: there it means the command's operation
// timeout, not the request timeout.
func extractGlobalFlags(arguments []string, settings *options) ([]string, error) {
	remaining := make([]string, 0, len(arguments))
	seenCommand := false
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			remaining = append(remaining, arguments[index:]...)
			break
		}
		if len(argument) < 2 || argument[0] != '-' {
			seenCommand = true
			remaining = append(remaining, argument)
			continue
		}
		name := strings.TrimLeft(argument, "-")
		raw, hasValue := "", false
		if before, after, found := strings.Cut(name, "="); found {
			name, raw, hasValue = before, after, true
		}
		switch name {
		case "json":
			value := true
			if hasValue {
				parsed, err := strconv.ParseBool(raw)
				if err != nil {
					return nil, fmt.Errorf("invalid value %q for flag --json", raw)
				}
				value = parsed
			}
			settings.jsonOutput = value
		case "agent", "control-url", "token-store":
			if !hasValue {
				if index+1 >= len(arguments) {
					return nil, fmt.Errorf("flag --%s needs a value", name)
				}
				index++
				raw = arguments[index]
			}
			switch name {
			case "agent":
				settings.agentEndpoint = raw
			case "control-url":
				settings.controlURL = raw
			case "token-store":
				settings.tokenStorePath = raw
			}
		case "timeout":
			if seenCommand {
				remaining = append(remaining, argument)
				if !hasValue && index+1 < len(arguments) {
					index++
					remaining = append(remaining, arguments[index])
				}
				continue
			}
			if !hasValue {
				if index+1 >= len(arguments) {
					return nil, errors.New("flag --timeout needs a value")
				}
				index++
				raw = arguments[index]
			}
			parsed, err := parseDurationOrSeconds(raw)
			if err != nil {
				return nil, fmt.Errorf("flag --timeout: %w", err)
			}
			settings.timeout = parsed
		default:
			remaining = append(remaining, argument)
		}
	}
	return remaining, nil
}

// helpRequest reports whether the command line asks for help (--help, -h, or
// the help command) and which topic it names. Everything after "--" belongs
// to a workload command and never counts; the word "help" in any later
// positional slot is data (a workload id), not a command.
func helpRequest(arguments []string) (topic string, asked bool) {
	topicSeen := ""
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			return "", false
		}
		switch argument {
		case "-h", "--help", "-help":
			return topicSeen, true
		case "help":
			if topicSeen != "" {
				break
			}
			if index+1 < len(arguments) && !strings.HasPrefix(arguments[index+1], "-") && arguments[index+1] != "--" {
				return arguments[index+1], true
			}
			return "", true
		}
		if !strings.HasPrefix(argument, "-") && topicSeen == "" {
			topicSeen = argument
		}
	}
	return "", false
}

// extractStringFlag pulls one value flag out of arguments wherever it
// appears (before "--"), so `fleet machines --org X list` works as well as
// `fleet --org X machines list`. The empty string means "not given".
func extractStringFlag(arguments []string, name string) (string, []string, error) {
	value := ""
	remaining := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			remaining = append(remaining, arguments[index:]...)
			break
		}
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			remaining = append(remaining, argument)
			continue
		}
		flagName := strings.TrimLeft(argument, "-")
		raw, hasValue := "", false
		if before, after, found := strings.Cut(flagName, "="); found {
			flagName, raw, hasValue = before, after, true
		}
		if flagName != name {
			remaining = append(remaining, argument)
			continue
		}
		if !hasValue {
			if index+1 >= len(arguments) {
				return "", nil, fmt.Errorf("flag --%s needs a value", name)
			}
			index++
			raw = arguments[index]
		}
		value = raw
	}
	return value, remaining, nil
}

// rejectArgs fails loudly on stray arguments instead of ignoring them. A
// stray flag gets the global-flag remedy; a stray positional gets the
// command's usage. Both are usage errors and exit 2.
func rejectArgs(command, usage string, arguments []string) error {
	if len(arguments) == 0 {
		return nil
	}
	if strings.HasPrefix(arguments[0], "-") {
		name := strings.TrimLeft(arguments[0], "-")
		if before, _, found := strings.Cut(name, "="); found {
			name = before
		}
		return usageError(unknownFlagError(command, arguments[0], name).Error())
	}
	return usageError(fmt.Sprintf("%s takes no arguments; usage: %s", command, usage))
}
