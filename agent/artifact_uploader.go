package agent

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/buildkite/agent/api"
	"github.com/buildkite/agent/glob"
	"github.com/buildkite/agent/logger"
	"github.com/buildkite/agent/pool"
	"github.com/buildkite/agent/retry"
)

type ArtifactUploader struct {
	// The APIClient that will be used when uploading jobs
	APIClient *api.Client

	// The ID of the Job
	JobID string

	// The path of the uploads
	Paths string

	// Where we'll be uploading artifacts
	Destination string
}

func (a *ArtifactUploader) Upload() error {
	// Create artifact structs for all the files we need to upload
	artifacts, err := a.collect()
	if err != nil {
		return err
	}

	if len(artifacts) == 0 {
		logger.Info("No files matched paths: %s", a.Paths)
	} else {
		logger.Info("Found %d files that match \"%s\"", len(artifacts), a.Paths)

		err := a.upload(artifacts)
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *ArtifactUploader) WorkingDirectory(path string) string {
	if filepath.IsAbs(path) {
		if runtime.GOOS == "windows" {
			return filepath.VolumeName(path)
		} else {
			return "/"
		}
	} else {
		dir, _ := os.Getwd()
		return dir
	}
}

func (a *ArtifactUploader) NormalizedPath(path string) string {
	return filepath.Join(a.WorkingDirectory(path), path)
}

func (a *ArtifactUploader) collect() (artifacts []*api.Artifact, err error) {
	globPaths := strings.Split(a.Paths, ";")

	for _, globPath := range globPaths {
		workingDirectory := a.WorkingDirectory(globPath)
		globPath = strings.TrimSpace(globPath)

		if globPath != "" {
			logger.Debug("Searching for %s", a.NormalizedPath(globPath))

			files, err := glob.Glob(workingDirectory, globPath)
			if err != nil {
				return nil, err
			}

			for _, file := range files {
				// Generate an absolute path for the artifact
				absolutePath, err := filepath.Abs(file)
				if err != nil {
					return nil, err
				}

				fileInfo, err := os.Stat(absolutePath)
				if fileInfo.IsDir() {
					logger.Debug("Skipping directory %s", file)
					continue
				}

				// Create a relative path (from the workingDirectory) to the artifact, by removing the
				// first part of the absolutePath that is the workingDirectory.
				relativePath := strings.Replace(absolutePath, workingDirectory, "", 1)

				// Ensure the relativePath doesn't have a file seperator "/" as the first character
				relativePath = strings.TrimPrefix(relativePath, string(os.PathSeparator))

				// Build an artifact object using the paths we have.
				artifact, err := a.build(relativePath, absolutePath, globPath)
				if err != nil {
					return nil, err
				}

				artifacts = append(artifacts, artifact)
			}
		}
	}

	return artifacts, nil
}

func (a *ArtifactUploader) build(relativePath string, absolutePath string, globPath string) (*api.Artifact, error) {
	// Temporarily open the file to get it's size
	file, err := os.Open(absolutePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Grab it's file info (which includes it's file size)
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}

	// Generate a sha1 checksum for the file
	hash := sha1.New()
	io.Copy(hash, file)
	checksum := fmt.Sprintf("%x", hash.Sum(nil))

	// Create our new artifact data structure
	artifact := &api.Artifact{
		State:        "new",
		Path:         relativePath,
		AbsolutePath: absolutePath,
		GlobPath:     globPath,
		FileSize:     fileInfo.Size(),
		Sha1Sum:      checksum,
	}

	return artifact, nil
}

func (a *ArtifactUploader) upload(artifacts []*api.Artifact) error {
	var uploader Uploader

	// Determine what uploader to use
	if a.Destination != "" {
		if strings.HasPrefix(a.Destination, "s3://") {
			uploader = new(S3Uploader)
		} else {
			return errors.New("Unknown upload destination: " + a.Destination)
		}
	} else {
		uploader = new(FormUploader)
	}

	// Setup the uploader
	err := uploader.Setup(a.Destination)
	if err != nil {
		return err
	}

	// Set the URL's of the artifacts based on the uploader
	for _, artifact := range artifacts {
		artifact.URL = uploader.URL(artifact)
	}

	// Create the artifacts on Buildkite
	batchCreator := ArtifactBatchCreator{
		APIClient: a.APIClient,
		JobID:     a.JobID,
		Artifacts: artifacts,
	}
	artifacts, err = batchCreator.Create()
	if err != nil {
		return err
	}

	p := pool.New(pool.MaxConcurrencyLimit)
	errors := []error{}

	for _, artifact := range artifacts {
		// Create new instance of the artifact for the goroutine
		// See: http://golang.org/doc/effective_go.html#channels
		artifact := artifact

		p.Spawn(func() {
			// Show a nice message that we're starting to upload the file
			logger.Info("Uploading \"%s\" %d bytes", artifact.Path, artifact.FileSize)

			// Upload the artifact and then set the state depending
			// on whether or not it passed. We'll retry the upload
			// a couple of times before giving up.
			err = retry.Do(func(s *retry.Stats) error {
				err := uploader.Upload(artifact)
				if err != nil {
					logger.Warn("%s (%s)", err, s)
				}

				return err
			}, &retry.Config{Maximum: 10, Interval: 1 * time.Second})
			if err != nil {
				artifact.State = "error"
				logger.Error("Error uploading artifact \"%s\": %s", artifact.Path, err)

				// Track the error that was raised
				p.Lock()
				errors = append(errors, err)
				p.Unlock()
			} else {
				artifact.State = "finished"
			}

			// Update the state of the artifact on Buildkite, we
			// retry this as well.
			err = retry.Do(func(s *retry.Stats) error {
				_, _, err = a.APIClient.Artifacts.Update(a.JobID, artifact)
				if err != nil {
					logger.Warn("%s (%s)", err, s)
				}

				return err
			}, &retry.Config{Maximum: 10, Interval: 1 * time.Second})
			if err != nil {
				logger.Error("Error marking artifact %s as uploaded: %s", artifact.Path, err)

				// Track the error that was raised
				p.Lock()
				errors = append(errors, err)
				p.Unlock()
			}
		})
	}

	p.Wait()

	if len(errors) > 0 {
		logger.Fatal("There were errors with uploading some of the artifacts")
	}

	return nil
}
