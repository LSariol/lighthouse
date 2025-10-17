package models

import "time"

type WatchedRepo struct {
	DisplayName   string    `json:"displayName"`
	ContainerName string    `json:"containerName"`
	URL           string    `json:"url"`
	APIURL        string    `json:"apiURL"`
	DownloadURL   string    `json:"downloadURL"`
	Stats         RepoStats `json:"stats"`
}

type RepoStats struct {
	Meta      MetaStats     `json:"meta"`
	Queries   QueryStats    `json:"queries"`
	Updates   UpdateStats   `json:"updates"`
	Builds    BuildStats    `json:"builds"`
	Downloads DownloadStats `json:"downloads"`
}

type MetaStats struct {
	StartedWatchingAt time.Time  `json:"startedWatchingAt"`
	LastModifiedAt    *time.Time `json:"lastModifiedAt"`
}

type QueryStats struct {
	LastQueriedAt    *time.Time `json:"lastQueriedAt"`
	QueryCount       int        `json:"queryCount"`
	LastErrorAt      *time.Time `json:"lastErrorAt"`
	LastErrorMessage *string    `json:"lastErrorMessage"`
}

type UpdateStats struct {
	LastUpdatedAt     *time.Time `json:"lastUpdatedAt"`
	LastSeenCommitSha *string    `json:"lastSeenCommitSha"`
	LastSeenTag       *string    `json:"lastSeenTag"`
	UpdateCount       int        `json:"updateCount"`
}

type BuildStats struct {
	LastBuildAt         *time.Time `json:"lastBuildAt"`
	LastBuildStatus     *string    `json:"lastBuildStatus"`
	BuildTriggeredCount int        `json:"buildTriggeredCount"`
}

type DownloadStats struct {
	LastDownloadAt         *time.Time `json:"lastDownloadAt"`
	LastDownloadStatus     *string    `json:"lastDownloadStatus"`
	DownloadTriggeredCount int        `json:"downloadTriggeredCount"`
}

func NewWatchedRepo(dName string, cName string, url string, apiURL string, downloadURL string) WatchedRepo {
	now := time.Now()

	return WatchedRepo{
		DisplayName:   dName,
		ContainerName: cName,
		URL:           url,
		APIURL:        apiURL,
		DownloadURL:   downloadURL,
		Stats: RepoStats{
			Meta: MetaStats{
				StartedWatchingAt: now,
				LastModifiedAt:    nil,
			},
			Queries: QueryStats{
				LastQueriedAt:    nil,
				QueryCount:       0,
				LastErrorAt:      nil,
				LastErrorMessage: nil,
			},
			Updates: UpdateStats{
				LastUpdatedAt:     nil,
				LastSeenCommitSha: nil,
				LastSeenTag:       nil,
				UpdateCount:       0,
			},
			Builds: BuildStats{
				LastBuildAt:         nil,
				LastBuildStatus:     nil,
				BuildTriggeredCount: 0,
			},
			Downloads: DownloadStats{
				LastDownloadAt:         nil,
				LastDownloadStatus:     nil,
				DownloadTriggeredCount: 0,
			},
		},
	}
}
