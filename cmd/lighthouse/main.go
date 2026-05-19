package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LSariol/LightHouse/internal/builder"
	"github.com/LSariol/LightHouse/internal/cli"
	"github.com/LSariol/LightHouse/internal/config"
	"github.com/LSariol/LightHouse/internal/watcher"
	"github.com/lsariol/coveclient"
	dockerclient "github.com/docker/docker/client"
)

func main() {

	var envPath string
	envPath, err := config.Load()
	if err != nil {
		panic(err)
	}

	// Build Dependencies
	var coveClient *coveclient.Client = watcher.NewCoveClient()
	fmt.Println(coveClient.ClientSecret)

	if err := config.SaveClientSecret(envPath, coveClient.ClientSecret); err != nil {
		panic(err)
	}

	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dockerClient, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}

	var builder *builder.Builder = builder.NewBuilder(dockerClient, coveClient, ctx)
	var watcher *watcher.Watcher = watcher.NewWatcher(coveClient, client, builder, ctx)

	builder.StartAllContainers()

	go watcher.Run()

	ok, err := builder.IsContainerRunning("cove")
	if err != nil {
		panic(err)
	}

	if !ok {
		builder.StartContainer("cove")
	}

	//__________________________________________________________

	// containers, _ := dockerHandler.ListContainers(ctx)

	// fmt.Println(containers)

	cmd := cli.NewCLI(watcher)
	go cmd.Run()

	<-ctx.Done()
	log.Println("Shutting Down...")

}
