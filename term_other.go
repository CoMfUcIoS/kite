//go:build !(darwin || linux)

package main

import "os"

func ttyColumns(*os.File) int { return 0 }
