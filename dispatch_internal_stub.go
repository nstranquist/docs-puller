//go:build !nicosinternal

package main

func dispatchInternalCommands(_ string, _ []string) bool {
	return false
}

func usageInternalLines() string {
	return ""
}
