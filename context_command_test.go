package main

import (
	"reflect"
	"testing"
)

func TestContextArgumentsAfterQuery(t *testing.T) {
	flags, words := splitContextArgs([]string{"react hooks", "--max-bytes", "4000", "--source", "react", "--lines=10"})
	if !reflect.DeepEqual(words, []string{"react hooks"}) || !reflect.DeepEqual(flags, []string{"--max-bytes", "4000", "--source", "react", "--lines=10"}) {
		t.Fatalf("flags=%v query=%v", flags, words)
	}
}
