package models

import "time"

func UpdateQueryStats(repo WatchedRepo) WatchedRepo {
	lastCheck := time.Now()
	repo.Stats.Queries.LastQueriedAt = &lastCheck
	repo.Stats.Queries.QueryCount += 1

	return repo
}

func UpdateUpdateStats(repo WatchedRepo, sha string) WatchedRepo {

	repo.Stats.Updates.LastSeenCommitSha = &sha
	currentTime := time.Now()
	repo.Stats.Updates.LastUpdatedAt = &currentTime
	repo.Stats.Updates.UpdateCount += 1

	return repo
}

func UpdateErrorStats(repo WatchedRepo, errorMessage string) WatchedRepo {
	currentTime := time.Now()
	repo.Stats.Queries.LastErrorAt = &currentTime
	repo.Stats.Queries.LastErrorMessage = &errorMessage

	return repo
}

func UpdateDownloadStats(repo WatchedRepo, downloadStatus string) WatchedRepo {

	repo.Stats.Downloads.DownloadTriggeredCount += 1
	repo.Stats.Downloads.LastDownloadStatus = &downloadStatus
	timeStamp := time.Now()
	repo.Stats.Downloads.LastDownloadAt = &timeStamp

	return repo
}

func UpdateBuildStats(repo WatchedRepo, buildStatus string) WatchedRepo {

	repo.Stats.Builds.BuildTriggeredCount += 1
	repo.Stats.Builds.LastBuildStatus = &buildStatus
	timeStamp := time.Now()
	repo.Stats.Builds.LastBuildAt = &timeStamp

	return repo
}

// ClearErrorStats forgets the last error, after a check that succeeded.
func ClearErrorStats(repo WatchedRepo) WatchedRepo {
	repo.Stats.Queries.LastErrorAt = nil
	repo.Stats.Queries.LastErrorMessage = nil

	return repo
}
