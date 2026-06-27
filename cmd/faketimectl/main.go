// Command faketimectl manages epochd fake-time operations.
// Controller subcommands talk to a running epochd controller (EPOCHD_URL or --url).
// Injection subcommands directly patch a process via ptrace (Linux only).
//
// Usage:
//
//	faketimectl [--url=EPOCHD_URL] <command> [flags] [args]
//
// Controller subcommands:
//
//	create   --namespace=NS --selector=SEL --time=RFC3339 [--ttl=DURATION]
//	list
//	get      <id>
//	update   <id> --time=RFC3339
//	advance  <id> --by=DURATION
//	delete   <id>
//	status   <id>
//	resolve  --namespace=NS --selector=SEL
//
// Local injection subcommands (Linux only, requires CAP_SYS_PTRACE or root):
//
//	inject   --pid=PID --time=RFC3339
//	reset    --pid=PID
//	run      --time=RFC3339 [--freeze] [--track] [--] COMMAND [ARGS]
package main

import (
	"fmt"
	"io"
	"os"
)

// stdout and stderr are package-level so tests can redirect them.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(stderr, "faketimectl: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "-h", "--help", "help":
		printUsage()
		return nil
	case "create":
		return cmdCreate(args[1:])
	case "list":
		return cmdList(args[1:])
	case "get":
		return cmdGet(args[1:])
	case "update":
		return cmdUpdate(args[1:])
	case "advance":
		return cmdAdvance(args[1:])
	case "delete":
		return cmdDelete(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "resolve":
		return cmdResolve(args[1:])
	case "inject":
		return cmdInject(args[1:])
	case "reset":
		return cmdReset(args[1:])
	case "run":
		return cmdRun(args[1:])
	default:
		return fmt.Errorf("unknown command %q\nRun 'faketimectl help' for usage", args[0])
	}
}

const usageText = `Usage: faketimectl <command> [flags]

Controller subcommands (require --url flag or EPOCHD_URL environment variable):
  create   --namespace=NS --selector=SEL --time=RFC3339 [--ttl=DURATION]
  list
  get      <id>
  update   <id> --time=RFC3339
  advance  <id> --by=DURATION
  delete   <id>
  status   <id>
  resolve  --namespace=NS --selector=SEL

Local injection subcommands (Linux only, requires CAP_SYS_PTRACE or root):
  inject   --pid=PID --time=RFC3339 [--freeze]
  reset    --pid=PID
  run      --time=RFC3339 [--freeze] [--track] [--] COMMAND [ARGS]`

func printUsage() {
	fmt.Fprintln(stdout, usageText)
}
