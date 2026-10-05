package watcher

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lsariol/coveclient"
)

func (w *Watcher) loadGitCredentials() error {

	fmt.Println("Getting Github token")

	gitToken, err := w.CC.GetSecret("LIGHTHOUSE_GITHUB_TOKEN")
	if err != nil {
		fmt.Println(err)
		return fmt.Errorf("loadGitCredentials: %v", err)
	}

	fmt.Println("GOT TOKEN")
	w.GitToken = gitToken
	return nil
}

func NewCoveClient() *coveclient.Client {

	coveClient := coveclient.New(os.Getenv("COVE_ADDRESS"), "")

	tokenPath := os.Getenv("COVE_TOKEN_PATH")
	if tokenPath == "" {
		panic("COVE_TOKEN_PATH is not set")
	}

	for {
		_, err := coveClient.LoadOrBootstrap(tokenPath)
		if err == nil {
			return coveClient
		}

		if !errors.Is(err, coveclient.ErrBootstrapClosed) {
			panic(err)
		}

		fmt.Println("Waiting for Cove token. Run 'bootstrap open lighthouse' in the Cove CLI. Retrying in 15s.")
		time.Sleep(15 * time.Second)
	}
}
