package cli

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/LSariol/LightHouse/internal/watcher"
)

type CLI struct {
	Watcher *watcher.Watcher
}

func NewCLI(w *watcher.Watcher) *CLI {

	return &CLI{
		Watcher: w,
	}
}

func (c *CLI) Run() {

	ioScanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("LightHouse CLI> ")
		if !ioScanner.Scan() {
			break
		}
		input := ioScanner.Text()
		c.parseCLI(strings.Fields(input))
	}
}

func (c *CLI) parseCLI(args []string) {

	if len(args) == 0 {
		return
	}

	switch args[0] {
	case "update", "u":

		switch args[1] {

		case "url", "URL":
			//do url update

		case "name", "NAME":
			//do name update

		}
	case "add", "a":

		if len(args) != 3 {
			fmt.Println("add requires 3 total arguments.")
			fmt.Println("add <DisplayName> <repoURL>")
			return
		}

		err := c.Watcher.AddNewRepo(args[1], args[2])
		if err != nil {
			fmt.Printf("Failed adding new repo: %w\n", err)
		}
		log.Printf("%s is now being watched.\n", args[1])

	case "remove", "r":

		if len(args) != 2 {
			fmt.Println("remove requires 2 total arguments.")
			fmt.Println("remove <repoName>")
			return
		}

		err := c.Watcher.RemoveRepo(args[1])
		if err != nil {
			fmt.Printf("Failed removing repo: %w\n", err)
		}

	case "change", "c":

		if len(args) != 3 {
			fmt.Println("remove requires 3 total arguments.")
			fmt.Println("change <old repository name> <new repo URL>")
			return
		}

		err := c.Watcher.UpdateRepo(args[1], args[2])
		if err != nil {
			fmt.Println("Failed to update Repository URL.")
		}

		fmt.Println("URL has been updated.")

		if strings.ToLower(args[1]) == "name" {
			err := c.Watcher.ChangeRepoName(args[2], args[3])
			if err != nil {
				fmt.Printf("Failed changing repo name for %s: %w\n", args[2], err)
			}

			fmt.Println("Name has been changed.")
			return
		}

	case "start", "START":
		if len(args) != 2 {
			fmt.Println("start requires 2 total arguments.")
			fmt.Println("start <projectName/ALL>")
			return
		}
		if args[1] == "ALL" || args[1] == "all" {
			err := c.Watcher.Builder.StartAllContainers()
			if err != nil {
				fmt.Printf("Error starting all containers: %w\n", err)
				return
			}
			fmt.Println("All Containers Started")
			return
		}

		err := c.Watcher.Builder.StartContainer(args[1])
		if err != nil {
			fmt.Printf("Error starting '%s': %w\n", args[1], err)
			return
		}
		fmt.Println("%w has been started.")
		return

	case "stop", "STOP":
		if len(args) != 2 {
			fmt.Println("stop requires 2 total arguments.")
			fmt.Println("stop <projectName/ALL>")
			return
		}
		if args[1] == "ALL" || args[1] == "all" {
			err := c.Watcher.Builder.StopAllContainers()
			if err != nil {
				fmt.Printf("Error starting all containers: %w\n", err)
				return
			}
			fmt.Println("All Containers stopped")
			return
		}

		err := c.Watcher.Builder.StopContainer(args[1])
		if err != nil {
			fmt.Printf("Error stopping '%s': %w\n", args[1], err)
			return
		}
		fmt.Println("%w has been stopped.")
		return

	case "scan", "SCAN":
		c.Watcher.Scan()
	case "list", "LIST", "l", "L":
		c.Watcher.DisplayWatchList()

	case "exit", "quit", "q":

		if len(args) == 1 {
			fmt.Println("Shutting down Lighthouse...")
			os.Exit(0)
			return
		}

		switch args[1] {
		case "all", "a":
			fmt.Println("Shutting down Lighthouse and all containers...")
			if err := c.Watcher.Builder.StopAllContainers(); err != nil {
				fmt.Printf("Error while shutting down containers: %v", err)
			}
			os.Exit(0)
		}

	}

	// 	case "get", "g":

	// 		if len(args) != 2 {
	// 			yellowLog("Get requires 2 total arguments.")
	// 			yellowLog("get <secret>")
	// 			return
	// 		}

	// 		if !ok {
	// 			redLog(res + ": " + args[1])
	// 			return
	// 		}

	// 		greenLog("Secret has been retreived: " + res)

	// 	case "add", "a":

	// 		if len(args) != 3 {
	// 			yellowLog("add requires 3 total arguments.")
	// 			yellowLog("add <secretName> <value>")
	// 			return
	// 		}

	// 		res, ok := encryption.AddSecret(args[1], args[2])
	// 		if !ok {
	// 			redLog(res)
	// 		}

	// 		greenLog("Secret has been added")

	// 	case "remove", "r", "delete", "d":

	// 		if len(args) != 2 {
	// 			yellowLog("Get requires 2 total arguments.")
	// 			yellowLog("remove <secret>")
	// 			return
	// 		}

	// 		res, ok := encryption.RemoveSecret(args[1])
	// 		if !ok {
	// 			redLog(res)
	// 			return
	// 		}

	// 		greenLog("Secret has been removed")

	// 	case "update", "u":

	// 		if len(args) != 3 {
	// 			yellowLog("Update requires 3 total arguments.")
	// 			yellowLog("update <secretName> <newValue>")
	// 			return
	// 		}

	// 		res, ok := encryption.UpdateSecret(args[1], args[2])
	// 		if !ok {
	// 			redLog(res)
	// 			return
	// 		}

	// 		greenLog("Secret has been updated.")

	// 	case "list", "l":

	// 		if len(args) > 2 {
	// 			yellowLog("Update requires 1 or 2 arguments.")
	// 			yellowLog("list or list all")
	// 			return
	// 		}

	// 		if len(args) == 1 {
	// 			displayWatchedRepos()
	// 			return
	// 		}

	// 		if len(args) == 2 || args[2] == "all" {

	// 		}

	// 	}
	// }

	// func greenLog(s string) {
	// 	fmt.Println("\033[32mCove CLI> " + s + "\033[0m")
	// }

	// func yellowLog(s string) {
	// 	fmt.Println("\033[33mCove CLI> " + s + "\033[0m")
	// }

	// func redLog(s string) {
	// 	fmt.Println("\033[31mCove CLI> " + s + "\033[0m")
	// }
}
