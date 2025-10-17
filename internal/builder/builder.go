package builder

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/LSariol/LightHouse/internal/models"
	"github.com/LSariol/coveclient"
	"github.com/docker/docker/client"
)

type Builder struct {
	Docker    *client.Client
	CC        *coveclient.Client
	Ctx       context.Context
	WatchList []models.WatchedRepo
	BasePath  string
}

func NewBuilder(dh *client.Client, cc *coveclient.Client, ctx context.Context) *Builder {
	return &Builder{
		Docker: dh,
		CC:     cc,
		Ctx:    ctx,
	}
}

func (b *Builder) Build(repo models.WatchedRepo) error {

	fmt.Println("----Building " + repo.ContainerName + " ----")

	err := cleanUp()
	if err != nil {
		wError := "Cleaning Failed for " + repo.ContainerName + " " + err.Error()
		fmt.Println(wError)
		return fmt.Errorf("cleanup: %w", err)
	}

	// Prepare Repo for build
	err = downloadNewCommit(repo.DownloadURL, repo.ContainerName)
	if err != nil {
		wError := "Download Failed for " + repo.ContainerName + " " + err.Error()
		fmt.Println(wError)
		return fmt.Errorf("download: %w", err)
	}

	err = b.StopContainer(repo.ContainerName)
	if err != nil {
		if strings.Contains(err.Error(), "No such container") {
			// ignore and continue
		} else {
			// propagate other errors
			return fmt.Errorf("build failed to stop container: %w", err)
		}

	}

	err = unpackNewProject(repo.ContainerName)
	if err != nil {
		wError := "Unzip Failed for " + repo.ContainerName + " " + err.Error()
		fmt.Println(wError)
		return fmt.Errorf("unpack: %w", err)
	}

	err = b.createContainer(strings.ToLower(repo.ContainerName))
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	err = cleanUp()
	if err != nil {
		wError := "Cleaning Failed for " + repo.ContainerName + " " + err.Error()
		fmt.Println(wError)
		return fmt.Errorf("cleanup end: %w", err)
	}

	fmt.Println("Clean Complete")

	return nil
}

func ErrorHandler() {

}

// Run containers if they already exist.
func (b *Builder) InitilizeContainers(watchList []models.WatchedRepo) error {

	for _, model := range watchList {
		// If container is running, good
		containerName := strings.ToLower(model.ContainerName)
		status, err := b.IsContainerRunning(containerName)
		if err != nil {
			return err
		}
		if status {
			fmt.Println(containerName + " is already running.")
			return nil
		}

		err = b.StartContainer(containerName)
		if err != nil {
			return err
		}
	}

	return nil
}

func InitilizeOriginalPath() string {
	originalPath, _ := os.Getwd()

	return originalPath
}

func (b *Builder) LoadPaths() error {

	b.BasePath = os.Getenv("BASE_PATH")

	return nil
}
