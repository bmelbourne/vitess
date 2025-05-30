/*
Copyright 2020 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package azblobbackupstorage implements the BackupStorage interface
// for Azure Blob Storage
package azblobbackupstorage

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-pipeline-go/pipeline"
	"github.com/Azure/azure-storage-blob-go/azblob"
	"github.com/spf13/pflag"

	"vitess.io/vitess/go/vt/mysqlctl/errors"
	"vitess.io/vitess/go/vt/utils"

	"vitess.io/vitess/go/viperutil"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/mysqlctl/backupstorage"
	"vitess.io/vitess/go/vt/servenv"
)

var (
	// This is the account name
	accountName = viperutil.Configure(
		configKey("account.name"),
		viperutil.Options[string]{
			EnvVars:  []string{"VT_AZBLOB_ACCOUNT_NAME"},
			FlagName: "azblob-backup-account-name",
		},
	)

	// This is the private access key
	accountKeyFile = viperutil.Configure(
		configKey("account.key_file"),
		viperutil.Options[string]{
			FlagName: "azblob-backup-account-key-file",
		},
	)

	// This is the name of the container that will store the backups
	containerName = viperutil.Configure(
		configKey("container_name"),
		viperutil.Options[string]{
			FlagName: "azblob-backup-container-name",
		},
	)

	// This is an optional prefix to prepend to all files
	storageRoot = viperutil.Configure(
		configKey("storage_root"),
		viperutil.Options[string]{
			FlagName: "azblob-backup-storage-root",
		},
	)

	azBlobBufferSize = viperutil.Configure(
		configKey("buffer-size"),
		viperutil.Options[int]{
			Default:  100 << (10 * 2), // 100 MiB
			FlagName: "azblob_buffer_size",
		},
	)

	azBlobParallelism = viperutil.Configure(
		configKey("parallelism"),
		viperutil.Options[int]{
			Default:  1,
			FlagName: "azblob-backup-parallelism",
		},
	)
)

const configKeyPrefix = "backup.storage.azblob"

var configKey = viperutil.KeyPrefixFunc(configKeyPrefix)

func registerFlags(fs *pflag.FlagSet) {
	accountNameValue := accountName.Get()
	accountKeyFileValue := accountKeyFile.Get()
	containerNameValue := containerName.Get()
	storageRootValue := storageRoot.Get()
	azBlobBufferSizeValue := azBlobBufferSize.Get()
	azBlobParallelismValue := azBlobParallelism.Get()

	utils.SetFlagStringVar(fs, &accountNameValue, "azblob-backup-account-name", accountName.Default(), "Azure Storage Account name for backups; if this flag is unset, the environment variable VT_AZBLOB_ACCOUNT_NAME will be used.")
	utils.SetFlagStringVar(fs, &accountKeyFileValue, "azblob-backup-account-key-file", accountKeyFile.Default(), "Path to a file containing the Azure Storage account key; if this flag is unset, the environment variable VT_AZBLOB_ACCOUNT_KEY will be used as the key itself (NOT a file path).")
	utils.SetFlagStringVar(fs, &containerNameValue, "azblob-backup-container-name", containerName.Default(), "Azure Blob Container Name.")
	utils.SetFlagStringVar(fs, &storageRootValue, "azblob-backup-storage-root", storageRoot.Default(), "Root prefix for all backup-related Azure Blobs; this should exclude both initial and trailing '/' (e.g. just 'a/b' not '/a/b/').")
	utils.SetFlagIntVar(fs, &azBlobBufferSizeValue, "azblob-backup-buffer-size", azBlobBufferSize.Default(), "The memory buffer size to use in bytes, per file or stripe, when streaming to Azure Blob Service.")
	utils.SetFlagIntVar(fs, &azBlobParallelismValue, "azblob-backup-parallelism", azBlobParallelism.Default(), "Azure Blob operation parallelism (requires extra memory when increased -- a multiple of azblob-backup-buffer-size).")

	viperutil.BindFlags(fs, accountName, accountKeyFile, containerName, storageRoot, azBlobParallelism)
}

func init() {
	servenv.OnParseFor("vtbackup", registerFlags)
	servenv.OnParseFor("vtctl", registerFlags)
	servenv.OnParseFor("vtctld", registerFlags)
	servenv.OnParseFor("vttablet", registerFlags)
}

const (
	defaultRetryCount = 5
	delimiter         = "/"
)

// Return a Shared credential from the available credential sources.
// We will use credentials in the following order
// 1. Direct Command Line Flag (azblob-backup-account-name, azblob-backup-account-key)
// 2. Environment variables
func azInternalCredentials() (string, string, error) {
	actName := accountName.Get()

	var actKey string
	if keyFile := accountKeyFile.Get(); keyFile != "" {
		log.Infof("Getting Azure Storage Account key from file: %s", keyFile)
		dat, err := os.ReadFile(keyFile)
		if err != nil {
			return "", "", err
		}
		actKey = string(dat)
	} else {
		actKey = os.Getenv("VT_AZBLOB_ACCOUNT_KEY")
	}

	if actName == "" || actKey == "" {
		return "", "", fmt.Errorf("Azure Storage Account credentials not found in command-line flags or environment variables")
	}
	return actName, actKey, nil
}

func azCredentials() (*azblob.SharedKeyCredential, error) {
	actName, actKey, err := azInternalCredentials()
	if err != nil {
		return nil, err
	}
	return azblob.NewSharedKeyCredential(actName, actKey)
}

func azServiceURL(credentials *azblob.SharedKeyCredential) azblob.ServiceURL {
	pipeline := azblob.NewPipeline(credentials, azblob.PipelineOptions{
		Retry: azblob.RetryOptions{
			Policy:   azblob.RetryPolicyFixed,
			MaxTries: defaultRetryCount,
			// Per https://godoc.org/github.com/Azure/azure-storage-blob-go/azblob#RetryOptions
			// this should be set to a very nigh number (they claim 60s per MB).
			// That could end up being days so we are limiting this to four hours.
			TryTimeout: 4 * time.Hour,
		},
		Log: pipeline.LogOptions{
			Log: func(level pipeline.LogLevel, message string) {
				switch level {
				case pipeline.LogFatal, pipeline.LogPanic:
					log.Fatal(message)
				case pipeline.LogError:
					log.Error(message)
				case pipeline.LogWarning:
					log.Warning(message)
				case pipeline.LogInfo, pipeline.LogDebug:
					log.Info(message)
				}
			},
			ShouldLog: func(level pipeline.LogLevel) bool {
				switch level {
				case pipeline.LogFatal, pipeline.LogPanic:
					return bool(log.V(3))
				case pipeline.LogError:
					return bool(log.V(3))
				case pipeline.LogWarning:
					return bool(log.V(2))
				case pipeline.LogInfo, pipeline.LogDebug:
					return bool(log.V(1))
				}
				return false
			},
		},
	})
	u := url.URL{
		Scheme: "https",
		Host:   credentials.AccountName() + ".blob.core.windows.net",
		Path:   "/",
	}
	return azblob.NewServiceURL(u, pipeline)
}

// AZBlobBackupHandle implements BackupHandle for Azure Blob service.
type AZBlobBackupHandle struct {
	bs        *AZBlobBackupStorage
	dir       string
	name      string
	readOnly  bool
	waitGroup sync.WaitGroup
	ctx       context.Context
	cancel    context.CancelFunc
	errors.PerFileErrorRecorder
}

// Directory implements BackupHandle.
func (bh *AZBlobBackupHandle) Directory() string {
	return bh.dir
}

// Name implements BackupHandle.
func (bh *AZBlobBackupHandle) Name() string {
	return bh.name
}

// AddFile implements BackupHandle.
func (bh *AZBlobBackupHandle) AddFile(ctx context.Context, filename string, filesize int64) (io.WriteCloser, error) {
	if bh.readOnly {
		return nil, fmt.Errorf("AddFile cannot be called on read-only backup")
	}
	// Error out if the file size it too large ( ~4.75 TB)
	maxSize := int64(azblob.BlockBlobMaxStageBlockBytes * azblob.BlockBlobMaxBlocks)
	if filesize > maxSize {
		return nil, fmt.Errorf("filesize (%v) is too large to upload to az blob (max size %v)", filesize, maxSize)
	}

	obj := objName(bh.dir, bh.name, filename)
	containerURL, err := bh.bs.containerURL()
	if err != nil {
		return nil, err
	}

	blockBlobURL := containerURL.NewBlockBlobURL(obj)

	reader, writer := io.Pipe()
	bh.waitGroup.Add(1)

	go func() {
		defer bh.waitGroup.Done()
		_, err := azblob.UploadStreamToBlockBlob(bh.ctx, reader, blockBlobURL, azblob.UploadStreamToBlockBlobOptions{
			BufferSize: azBlobBufferSize.Get(),
			MaxBuffers: azBlobParallelism.Get(),
		})
		if err != nil {
			reader.CloseWithError(err)
			bh.RecordError(filename, err)
		}
	}()

	return writer, nil
}

// EndBackup implements BackupHandle.
func (bh *AZBlobBackupHandle) EndBackup(ctx context.Context) error {
	if bh.readOnly {
		return fmt.Errorf("EndBackup cannot be called on read-only backup")
	}
	bh.waitGroup.Wait()
	return bh.Error()
}

// AbortBackup implements BackupHandle.
func (bh *AZBlobBackupHandle) AbortBackup(ctx context.Context) error {
	if bh.readOnly {
		return fmt.Errorf("AbortBackup cannot be called on read-only backup")
	}
	// Cancel the context of any uploads.
	bh.cancel()

	// Remove the backup
	return bh.bs.RemoveBackup(ctx, bh.dir, bh.name)
}

// ReadFile implements BackupHandle.
func (bh *AZBlobBackupHandle) ReadFile(ctx context.Context, filename string) (io.ReadCloser, error) {
	if !bh.readOnly {
		return nil, fmt.Errorf("ReadFile cannot be called on read-write backup")
	}

	obj := objName(bh.dir, filename)
	containerURL, err := bh.bs.containerURL()
	if err != nil {
		return nil, err
	}
	blobURL := containerURL.NewBlobURL(obj)

	resp, err := blobURL.Download(ctx, 0, azblob.CountToEnd, azblob.BlobAccessConditions{}, false, azblob.ClientProvidedKeyOptions{})
	if err != nil {
		return nil, err
	}
	return resp.Body(azblob.RetryReaderOptions{
		MaxRetryRequests: defaultRetryCount,
		NotifyFailedRead: func(failureCount int, lastError error, offset int64, count int64, willRetry bool) {
			log.Warningf("ReadFile: [azblob] container: %s, directory: %s, filename: %s, error: %v", containerName, objName(bh.dir, ""), filename, lastError)
		},
		TreatEarlyCloseAsError: true,
	}), nil
}

// AZBlobBackupStorage structs implements the BackupStorage interface for AZBlob
type AZBlobBackupStorage struct {
}

func (bs *AZBlobBackupStorage) containerURL() (*azblob.ContainerURL, error) {
	credentials, err := azCredentials()
	if err != nil {
		return nil, err
	}
	u := azServiceURL(credentials).NewContainerURL(containerName.Get())
	return &u, nil
}

// ListBackups implements BackupStorage.
func (bs *AZBlobBackupStorage) ListBackups(ctx context.Context, dir string) ([]backupstorage.BackupHandle, error) {
	var searchPrefix string
	if dir == "/" {
		searchPrefix = "/"
	} else {
		searchPrefix = objName(dir, "")
	}

	log.Infof("ListBackups: [azblob] container: %s, directory: %v", containerName, searchPrefix)

	containerURL, err := bs.containerURL()
	if err != nil {
		return nil, err
	}

	result := make([]backupstorage.BackupHandle, 0)
	var subdirs []string

	for marker := (azblob.Marker{}); marker.NotDone(); {
		// This returns Blobs in sorted order so we don't need to sort them a second time.
		resp, err := containerURL.ListBlobsHierarchySegment(ctx, marker, delimiter, azblob.ListBlobsSegmentOptions{
			Prefix:     searchPrefix,
			MaxResults: 0,
		})

		if err != nil {
			return nil, err
		}

		for _, item := range resp.Segment.BlobPrefixes {
			subdir := strings.TrimPrefix(item.Name, searchPrefix)
			subdir = strings.TrimSuffix(subdir, delimiter)
			subdirs = append(subdirs, subdir)
		}

		marker = resp.NextMarker
	}

	for _, subdir := range subdirs {
		cancelableCtx, cancel := context.WithCancel(ctx)
		result = append(result, &AZBlobBackupHandle{
			bs:       bs,
			dir:      strings.Join([]string{dir, subdir}, "/"),
			name:     subdir,
			readOnly: true,
			ctx:      cancelableCtx,
			cancel:   cancel,
		})
	}

	return result, nil
}

// StartBackup implements BackupStorage.
func (bs *AZBlobBackupStorage) StartBackup(ctx context.Context, dir, name string) (backupstorage.BackupHandle, error) {
	cancelableCtx, cancel := context.WithCancel(ctx)
	return &AZBlobBackupHandle{
		bs:       bs,
		dir:      dir,
		name:     name,
		readOnly: false,
		ctx:      cancelableCtx,
		cancel:   cancel,
	}, nil
}

// RemoveBackup implements BackupStorage.
func (bs *AZBlobBackupStorage) RemoveBackup(ctx context.Context, dir, name string) error {
	log.Infof("ListBackups: [azblob] container: %s, directory: %s", containerName, objName(dir, ""))

	containerURL, err := bs.containerURL()
	if err != nil {
		return err
	}

	searchPrefix := objName(dir, name, "")

	for marker := (azblob.Marker{}); marker.NotDone(); {
		resp, err := containerURL.ListBlobsHierarchySegment(ctx, marker, delimiter, azblob.ListBlobsSegmentOptions{
			Prefix:     searchPrefix,
			MaxResults: 0,
		})

		if err != nil {
			return err
		}

		// Right now there is no batch delete so we must iterate over all the blobs to delete them one by one
		// One day we will be able to use this https://docs.microsoft.com/en-us/rest/api/storageservices/blob-batch
		// but currently it is listed as a preview and its not in the go API
		for _, item := range resp.Segment.BlobItems {
			_, err := containerURL.NewBlobURL(item.Name).Delete(ctx, azblob.DeleteSnapshotsOptionInclude, azblob.BlobAccessConditions{})
			if err != nil {
				return err
			}
		}
		marker = resp.NextMarker
	}

	// Delete the blob representing the folder of the backup, remove any trailing slash to signify we want to remove the folder
	// NOTE: you must set DeleteSnapshotsOptionNone or this will error out with a server side error
	for retry := 0; retry < defaultRetryCount; retry = retry + 1 {
		// Since the deletion of blob's is asyncronious we may need to wait a bit before we delete the folder
		// Also refresh the client just for good measure
		time.Sleep(10 * time.Second)
		containerURL, err = bs.containerURL()
		if err != nil {
			return err
		}

		log.Infof("Removing backup directory: %v", strings.TrimSuffix(searchPrefix, "/"))
		_, err = containerURL.NewBlobURL(strings.TrimSuffix(searchPrefix, "/")).Delete(ctx, azblob.DeleteSnapshotsOptionNone, azblob.BlobAccessConditions{})
		if err == nil {
			break
		}
	}
	return err
}

// Close implements BackupStorage.
func (bs *AZBlobBackupStorage) Close() error {
	// This function is a No-op
	return nil
}

func (bs *AZBlobBackupStorage) WithParams(params backupstorage.Params) backupstorage.BackupStorage {
	// TODO(maxeng): return a new AZBlobBackupStorage that uses params.
	return bs
}

// objName joins path parts into an object name.
// Unlike path.Join, it doesn't collapse ".." or strip trailing slashes.
// It also adds the value of the -azblob-backup-storage-root flag if set.
func objName(parts ...string) string {
	if root := storageRoot.Get(); root != "" {
		return root + "/" + strings.Join(parts, "/")
	}
	return strings.Join(parts, "/")
}

func init() {
	backupstorage.BackupStorageMap["azblob"] = &AZBlobBackupStorage{}
}
