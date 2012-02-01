package main

import (
	"fmt"
	"launchpad.net/juju/go/log"
	"os"
)

func main() {
	Main(os.Args)
}

func Main(args []string) {
	jc := NewJujuCommand()
	jc.Register(&BootstrapCommand{})

	if err := Parse(jc, false, args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		PrintUsage(jc)
		os.Exit(2)
	}
	if err := jc.Run(); err != nil {
		log.Debugf("%s command failed: %s\n", jc.Info().Name, err)
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}
