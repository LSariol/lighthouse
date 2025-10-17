package watcher

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/LSariol/LightHouse/internal/models"
)

func (w *Watcher) AddNewRepo(displayName string, url string) error {

	// Check if new URL is already being watched
	exists := w.repoExists(displayName, url)
	if exists {
		fmt.Printf("%s is already being watched.\n", url)
		return nil
	}

	rName, rAPIURL, rDownloadURL, err := parseURL(url)
	if err != nil {
		return err
	}

	newRepo := models.NewWatchedRepo(displayName, rName, url, rAPIURL, rDownloadURL)

	w.WatchList = append(w.WatchList, newRepo)

	w.storeWatchList()

	return nil

}

func (w *Watcher) RemoveRepo(toRemove string) error {

	indexToRemove := -1

	for index, existingRepo := range w.WatchList {
		if existingRepo.DisplayName == toRemove {
			indexToRemove = index
			break
		}
	}

	if indexToRemove != -1 {
		w.WatchList = append(w.WatchList[:indexToRemove], w.WatchList[indexToRemove+1:]...)
		fmt.Printf("%s has been removed from the watchlist.\n", toRemove)
		w.storeWatchList()
		return nil
	}

	return fmt.Errorf("unable to remove %s from watchlist", toRemove)

}

func (w *Watcher) ChangeRepoName(currentName string, name string) error {
	updated := false

	if w.checkNamingConflicts(name, currentName) {
		return fmt.Errorf("this name is already being used to watch a different repo")
	}

	for i := range w.WatchList {
		if w.WatchList[i].DisplayName == currentName {
			w.WatchList[i].DisplayName = name
			lastModified := time.Now()
			w.WatchList[i].Stats.Meta.LastModifiedAt = &lastModified
			updated = true
			break
		}
	}

	if !updated {
		return fmt.Errorf("changeRepoName: %s does not exist", currentName)
	}

	w.storeWatchList()

	return nil
}

func (w *Watcher) ChangeRepoURL(dName string, newURL string) error {
	updated := false

	if w.checkURLConflicts(dName, newURL) {
		return fmt.Errorf("this url is already being watched under a different name")
	}

	for i := range w.WatchList {
		if w.WatchList[i].DisplayName == dName {

			_, apiURL, downloadURL, err := parseURL(newURL)
			if err != nil {
				return fmt.Errorf("changeRepoURL: %w", err)
			}

			w.WatchList[i].URL = newURL
			lastModified := time.Now()
			w.WatchList[i].Stats.Meta.LastModifiedAt = &lastModified
			w.WatchList[i].APIURL = apiURL
			w.WatchList[i].DownloadURL = downloadURL
			w.WatchList[i].DisplayName = dName
			updated = true
			break
		}
	}

	if !updated {
		return fmt.Errorf("changeRepoURL: %s does not exist", dName)
	}

	w.storeWatchList()

	return nil
}

func (w *Watcher) UpdateRepo(dName string, newURL string) error {
	updated := false

	if w.isRepoWatched(newURL) {
		return fmt.Errorf("this url is already being watched")
	}

	for i := range w.WatchList {
		if w.WatchList[i].DisplayName == dName {

			_, apiURL, downloadURL, err := parseURL(newURL)
			if err != nil {
				return fmt.Errorf("changeRepoURL: %w", err)
			}

			w.WatchList[i].URL = newURL
			lastModified := time.Now()
			w.WatchList[i].Stats.Meta.LastModifiedAt = &lastModified
			w.WatchList[i].APIURL = apiURL
			w.WatchList[i].DownloadURL = downloadURL
			updated = true
			break
		}
	}

	if !updated {
		return fmt.Errorf("changeRepoURL: %s does not exist", dName)
	}

	w.storeWatchList()

	return nil
}

//Helper Functions

// Returns a boolean if repo exists
func (w *Watcher) repoExists(displayName string, url string) bool {

	for _, existingRepo := range w.WatchList {
		if existingRepo.URL == url {
			return true
		}
		if existingRepo.DisplayName == displayName {
			return true
		}
	}

	return false

}

func (w *Watcher) checkNamingConflicts(name string, currentName string) bool {

	for _, repo := range w.WatchList {
		if repo.DisplayName == name && repo.DisplayName != currentName {
			return true
		}
	}

	return false
}

func (w *Watcher) checkURLConflicts(name string, currentURL string) bool {

	for _, repo := range w.WatchList {
		if repo.URL == currentURL && repo.DisplayName != name {
			return true
		}
	}

	return false
}

// checkWatchedReposConflicts verifies that the new url is not currently being watched.
func (w *Watcher) isRepoWatched(currentURL string) bool {

	for _, repo := range w.WatchList {
		if repo.URL == currentURL {
			return true
		}
	}

	return false
}

func (w *Watcher) loadWatchList() error {
	var watchList []models.WatchedRepo

	//read json file
	data, err := os.ReadFile(os.Getenv("APP_REPO_PATH"))
	if err != nil {
		fmt.Println("Watcher - LoadWatchList: Failed to load repos.json")
		return fmt.Errorf("loadWatchList: %w", err)
	}

	//Unmarshal repos.json into watchList
	err = json.Unmarshal([]byte(data), &watchList)
	if err != nil {
		fmt.Println("Watcher - LoadWatchList: Failed to Unmarshal json into WatchedRepos.")
		return fmt.Errorf("loadWatchList: %w", err)
	}

	w.WatchList = watchList
	w.Builder.WatchList = watchList
	return nil
}

func (w *Watcher) storeWatchList() {

	updatedData, err := json.MarshalIndent(w.WatchList, "", "	")
	if err != nil {
		fmt.Println("Watcher - storeWatchList: Failed to Marhsal json into UpdatedData.")
		return
	}

	err = os.WriteFile(os.Getenv("APP_REPO_PATH"), updatedData, 0644)
	if err != nil {
		fmt.Println("Watcher - storeWatchList: Failed to write to repos.json." + err.Error())
		return
	}
}

// Display WatchList in a nice format
func (w *Watcher) DisplayWatchList() {

	fmt.Printf("%-20s | %-40s | %-20s | %-15s\n", "Name", "URL", "Started Watching", "Query Count")
	fmt.Println(strings.Repeat("-", 20) + "-+-" + strings.Repeat("-", 40) + "-+-" + strings.Repeat("-", 20) + "-+-" + strings.Repeat("-", 15))

	for _, repo := range w.WatchList {
		fmt.Printf(
			"%-20s | %-40s | %-20s | %-15d \n",
			repo.DisplayName,
			repo.URL,
			repo.Stats.Meta.StartedWatchingAt.Format("2006-01-02 15:04:05"),
			repo.Stats.Queries.QueryCount,
		)
	}
}

func parseURL(url string) (string, string, string, error) {

	trim := strings.TrimPrefix(url, "https://github.com/")
	parts := strings.Split(trim, "/")
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid Github repo URL: %s", url)
	}

	rOwner := parts[0]
	rName := parts[1]

	rAPIURL := "https://api.github.com/repos/" + rOwner + "/" + rName
	rDownloadURL := "https://github.com/" + rOwner + "/" + rName + "/archive/refs/heads/main.zip"

	return rName, rAPIURL, rDownloadURL, nil

}
