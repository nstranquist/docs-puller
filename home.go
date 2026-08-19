package main

import "github.com/nstranquist/docs-puller/internal/apppaths"

func userHomeDir() (string, error) {
	return apppaths.UserHomeDir()
}
