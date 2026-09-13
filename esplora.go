package main

import "regexp"

var txidRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
