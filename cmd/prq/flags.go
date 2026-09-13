package main

import (
	"flag"
	"fmt"
	"strings"
)

// Preserve option values that happen to be spelled --json.
func extractJSON(args []string) (bool, []string) {
	jsonMode := false
	var result []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			result = append(result, args[i:]...)
			break
		}
		if arg == "--json" {
			jsonMode = true
			continue
		}
		result = append(result, arg)
		switch strings.TrimLeft(arg, "-") {
		case "repo", "pr", "status", "event", "reason":
			if strings.HasPrefix(arg, "-") && i+1 < len(args) {
				i++
				result = append(result, args[i])
			}
		}
	}
	return jsonMode, result
}

// Support the documented positional-then-options syntax using standard flag types.
func parseFlags(fs *flag.FlagSet, args []string) error {
	var options, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := fs.Lookup(name)
		if f == nil {
			return fmt.Errorf("unknown option %s", arg)
		}
		options = append(options, arg)
		boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool })
		if !hasValue && (!ok || !boolFlag.IsBoolFlag()) {
			if i+1 == len(args) {
				return fmt.Errorf("option %s needs a value", arg)
			}
			i++
			options = append(options, args[i])
		}
	}
	return fs.Parse(append(append(options, "--"), positional...))
}
