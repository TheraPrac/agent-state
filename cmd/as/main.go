package main

import "os"

func main() {
	app := newApp("")
	if err := app.Execute(); err != nil {
		os.Exit(1)
	}
	os.Exit(exitCode)
}
