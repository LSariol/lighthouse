package watcher

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/LSariol/LightHouse/internal/builder"
	"github.com/LSariol/LightHouse/internal/models"
	"github.com/lsariol/coveclient"
)

type Watcher struct {
	CC        *coveclient.Client
	HTTP      *http.Client
	Builder   *builder.Builder
	Ctx       context.Context
	WatchList []models.WatchedRepo
	HomePath  string
	GitToken  string
}

func NewWatcher(cloveClient *coveclient.Client, http *http.Client, builder *builder.Builder, ctx context.Context) *Watcher {
	return &Watcher{
		CC:      cloveClient,
		HTTP:    http,
		Builder: builder,
		Ctx:     ctx,
	}
}

func (w *Watcher) Run() error {

	err := w.loadWatchList()
	if err != nil {
		return err
	}

	w.loadGitCredentials()
	if err != nil {
		return err
	}

	for {

		if err := w.Scan(); err != nil {
			fmt.Printf("ERROR IN SCAN: %v", err)
		}
		time.Sleep(10 * time.Second)

	}

}

func (w *Watcher) Scan() error {

	for i, repo := range w.WatchList {
		repo = w.WatchList[i]
		currentHash, err := w.getLatestSHA(repo.APIURL, w.GitToken)
		if err != nil {

			repo = models.UpdateErrorStats(repo, err.Error())
			repo = models.UpdateQueryStats(repo)
			w.WatchList[i] = repo
			w.storeWatchList()
			return fmt.Errorf("scanner.scan() - getLatestSha: %v", err)

		}

		if repo.Stats.Updates.LastSeenCommitSha == nil || *repo.Stats.Updates.LastSeenCommitSha != currentHash {

			repo = models.UpdateUpdateStats(repo, currentHash)
			err := w.Builder.Build(repo)
			if err != nil {
				builder.ErrorHandler()
				return fmt.Errorf("scanner.scan() - error in build: %v", err)
			}

		}

		repo = models.UpdateQueryStats(repo)
		w.WatchList[i] = repo
		w.storeWatchList()

		continue
	}

	return nil

}
