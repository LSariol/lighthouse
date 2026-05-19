package watcher

import (
	"fmt"
	"os"

	"github.com/lsariol/coveclient"
)

func (w *Watcher) loadGitCredentials() error {

	fmt.Println("Getting GITHUB PAT")

	gitToken, err := w.CC.GetSecret("LIGHTHOUSE_GITHUB_PAT")
	if err != nil {
		fmt.Println(err)
		return fmt.Errorf("loadGitCredentials: %v", err)
	}

	fmt.Println("GOT TOKEN")
	w.GitToken = gitToken
	return nil
}

func NewCoveClient() *coveclient.Client {

	clientSecret := os.Getenv("COVE_CLIENT_SECRET")
	var coveClient *coveclient.Client = coveclient.New(os.Getenv("COVE_ADDRESS"), clientSecret, "lighthouse")

	if clientSecret == "" {
		clientSecret, err := coveClient.Bootstrap()
		if err != nil {
			panic(err)
		}
		coveClient.ClientSecret = clientSecret
	}

	return coveClient
}
