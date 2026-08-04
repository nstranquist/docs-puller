package main

import (
	"fmt"
	"strings"
)

// normalizeSinglePositionalArgs lets a command accept one positional
// argument before or after flags. The standard flag package stops parsing at
// the first positional argument, but documented commands use both forms.
func normalizeSinglePositionalArgs(args []string, valueFlags map[string]bool, description string) ([]string, error) {
	normalized := make([]string, 0, len(args))
	var positional string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 >= len(args) || positional != "" || i+2 != len(args) {
				return nil, fmt.Errorf("one %s is required", description)
			}
			positional = args[i+1]
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if positional != "" {
				return nil, fmt.Errorf("one %s is required", description)
			}
			positional = arg
			continue
		}
		flagName := strings.TrimLeft(arg, "-")
		if name, _, ok := strings.Cut(flagName, "="); ok {
			flagName = name
		}
		normalized = append(normalized, arg)
		if strings.Contains(arg, "=") || !valueFlags[flagName] {
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("-%s requires a value", flagName)
		}
		normalized = append(normalized, args[i+1])
		i++
	}
	if positional == "" {
		return nil, fmt.Errorf("one %s is required", description)
	}
	return append(normalized, positional), nil
}
