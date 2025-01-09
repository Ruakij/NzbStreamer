package main

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"astuart.co/nntp"
	"golang.org/x/exp/maps"
	"golang.org/x/sync/errgroup"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nntpClient"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation/webdav"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/SimpleWebdavFilesystem"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskCache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameOps"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbParser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/AdaptiveParallelMergerResource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/AdaptiveReadaheadCache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/RarFileResource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/SevenzipFileResource"

	gowebdav "github.com/emersion/go-webdav"
)

const (
	usenetHost    string = ""
	usenetPort    int    = 563
	usenetTls     bool   = true
	usenetUser    string = ""
	usenetPass    string = ""
	usenetMaxConn int    = 20
)

const (
	webdavAdress string = ":8080"
)

const (
	CacheLocation string = "../../.cache"
	CacheMaxSize  int    = 20 * 1024 * 1024 * 1024
)

const (
	// Over how much time average speed is calculated
	AdaptiveReadaheadCacheAvgSpeedTime time.Duration = 2 * time.Second
	// How far ahead to read in time
	AdaptiveReadaheadCacheTime time.Duration = 2 * time.Second
	// Lowest expected speed					Can help at beginning speeding up
	AdaptiveReadaheadCacheMinSize int = 512 * 1024
	// Low-Buffer (When to load more data)		Helps with continous data-flow
	AdaptiveReadaheadCacheLowBuffer int = 1 * 1024 * 1024
	// Max expected speed + Low-Buffer			Max speed per iteration
	AdaptiveReadaheadCacheMaxSize int = 16 * 1024 * 1024
)

const (
	FilesystemDisplayExtractedContainers bool = false
)

const (
	ReplaceBaseFilenameWithNzbBelowFuzzyThreshold float32 = 0.2
)

var (
	filesystem   *SimpleWebdavFilesystem.FS = SimpleWebdavFilesystem.NewFS()
	segmentCache *diskCache.Cache
)

func main() {
	var err error = nil

	// Setup nntpClient
	nntpClient := nntpClient.SetupNntpClient(usenetHost, usenetPort, usenetTls, usenetUser, usenetPass, usenetMaxConn)

	// Setup cache
	segmentCache, err = diskCache.NewCache(&diskCache.CacheOptions{
		CacheDir:             CacheLocation,
		MaxSize:              int64(CacheMaxSize),
		MaxSizeEvictBlocking: false,
	})
	if err != nil {
		panic(err)
	}

	nzbWatchFolder := "../../.testfiles/nzbs"
	files, err := os.ReadDir(nzbWatchFolder)
	if err != nil {
		panic(err)
	}
	nzbFiles := make([]string, 0, len(files))
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		name := file.Name()
		if !strings.HasSuffix(name, ".nzb") {
			continue
		}
		p := path.Join(nzbWatchFolder, name)
		nzbFiles = append(nzbFiles, p)
	}

	fmt.Printf("Loading %d Nzb-Files\n", len(nzbFiles))
	var group errgroup.Group
	for _, nzbFile := range nzbFiles {
		group.Go(func() (err error) {
			// Load file
			nzbData, err := loadNzbFile(nzbFile)
			if err != nil {
				return
			}

			// Add as files
			err = createResources(filesystem, nzbData, segmentCache, nntpClient, FilesystemExcludedExtensions)
			if err != nil {
				return
			}

			return
		})
	}
	err = group.Wait()
	if err != nil {
		panic(err)
	}

	// Serve webdav
	err = webdav.Listen(webdavAdress, &gowebdav.Handler{
		FileSystem: filesystem,
	})
	if err != nil {
		panic(err)
	}
}

func loadNzbFile(path string) (*nzbParser.NzbData, error) {
	nzbFile, err := os.OpenFile(path, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, fmt.Errorf("Couldnt read nzbfile: %v\n", err)
	}

	nzbData, err := nzbParser.ParseNzb(nzbFile)
	if err != nil {
		return nil, fmt.Errorf("Failed parsing nzbfile: %v\n", err)
	}

	warnings, errors := nzbData.CheckPlausability()
	if len(warnings) > 0 {
		fmt.Println("Warnings while checking Nzb:")
		for _, warning := range warnings {
			fmt.Println(warning)
		}
	}
	if len(errors) > 0 {
		var errMsg string
		for i, err := range errors {
			if i != len(errors)-1 {
				errMsg += fmt.Sprintf("%v, ", err)
			} else {
				errMsg += fmt.Sprintf("%v", err)
			}
		}
		return nil, fmt.Errorf("errors while checking Nzb: %s", errMsg)
	}
	return nzbData, nil
}

func createResources(filesystem *SimpleWebdavFilesystem.FS, nzbData *nzbParser.NzbData, cache *diskCache.Cache, nntpClient *nntp.Client) (err error) {
	namedFileResources := BuildNamedFileResourcesFromNzb(nzbData, cache, nntpClient)

	// Extract filenames
	filenames := make([]string, 0, len(namedFileResources))
	for filename := range namedFileResources {
		filenames = append(filenames, filename)
	}

	// Group
	groupedFilenames := filenameOps.GroupPartFilenames(filenames)
	//  Sort
	filenameOps.SortGroupedFilenames(groupedFilenames)

	// Add to filesystem
	for groupFilename, filenames := range groupedFilenames {
		if strings.HasSuffix(groupFilename, ".par2") {
			continue
		}

		extractableContainer := false

		// Special extensions
		if strings.HasSuffix(groupFilename, ".rar") {
			resources := make([]resource.ReadSeekCloseableResource, len(filenames))
			for i, filename := range filenames {
				resources[i] = namedFileResources[filename]
			}

			resource := RarFileResource.NewRarFileResource(resources, nzbData.Meta[nzbParser.MetaKeyPassword], "")
			fileheaders, err := resource.GetRarFiles(1)
			if err != nil {
				return err
			}

			extractableContainer = true

			for _, fileheader := range fileheaders {
				filename := fileheader.Name

				resource := RarFileResource.NewRarFileResource(resources, nzbData.Meta[nzbParser.MetaKeyPassword], filename)

				// Add readaheadCache
				readaheadResource := AdaptiveReadaheadCache.NewAdaptiveReadaheadCache(resource, AdaptiveReadaheadCacheAvgSpeedTime, AdaptiveReadaheadCacheTime, AdaptiveReadaheadCacheMinSize, AdaptiveReadaheadCacheMaxSize, AdaptiveReadaheadCacheLowBuffer)

				path := nzbData.Meta["Name"] + "/" + groupFilename
				if len(fileheaders) == 1 {
					filename = filenameOps.GetOrDefaultWithExtensionBelowLevensteinSimilarity(filename, nzbData.Meta["Name"], ReplaceBaseFilenameWithNzbBelowFuzzyThreshold)
				}
				filesystem.AddFile(path, filename, fileheader.ModificationTime, readaheadResource)
			}
		} else if strings.HasSuffix(groupFilename, ".7z") {
			extractableContainer = true

			resources := make([]resource.ReadSeekCloseableResource, len(filenames))
			for i, filename := range filenames {
				resources[i] = namedFileResources[filename]
			}

			mergedResource := AdaptiveParallelMergerResource.NewAdaptiveParallelMergerResource(resources)

			resource := SevenzipFileResource.NewSevenzipFileResource(mergedResource, nzbData.Meta[nzbParser.MetaKeyPassword], "")
			files, err := resource.GetFiles()
			if err != nil {
				return err
			}

			for path, fileinfo := range files {
				filename := fileinfo.Name()

				resource := SevenzipFileResource.NewSevenzipFileResource(mergedResource, nzbData.Meta[nzbParser.MetaKeyPassword], filename)

				// Add readaheadCache
				readaheadResource := AdaptiveReadaheadCache.NewAdaptiveReadaheadCache(resource, AdaptiveReadaheadCacheAvgSpeedTime, AdaptiveReadaheadCacheTime, AdaptiveReadaheadCacheMinSize, AdaptiveReadaheadCacheMaxSize, AdaptiveReadaheadCacheLowBuffer)

				path = nzbData.Meta["Name"] + "/" + groupFilename + "/" + path
				if len(files) == 1 {
					filename = filenameOps.GetOrDefaultWithExtensionBelowLevensteinSimilarity(filename, nzbData.Meta["Name"], ReplaceBaseFilenameWithNzbBelowFuzzyThreshold)
				}
				filesystem.AddFile(path, filename, fileinfo.ModTime(), readaheadResource)
			}
		}

		if !extractableContainer || FilesystemDisplayExtractedContainers {
			for _, filename := range filenames {
				resource := namedFileResources[filename]

				// Add readaheadCache
				readaheadResource := AdaptiveReadaheadCache.NewAdaptiveReadaheadCache(resource, AdaptiveReadaheadCacheAvgSpeedTime, AdaptiveReadaheadCacheTime, AdaptiveReadaheadCacheMinSize, AdaptiveReadaheadCacheMaxSize, AdaptiveReadaheadCacheLowBuffer)

				path := nzbData.Meta["Name"]
				if len(filenames) == 1 {
					filename = filenameOps.GetOrDefaultWithExtensionBelowLevensteinSimilarity(filename, nzbData.Meta["Name"], ReplaceBaseFilenameWithNzbBelowFuzzyThreshold)
				}
				filesystem.AddFile(path, filename, nzbData.Files[0].ParsedDate, readaheadResource)
			}
		}
	}

	return
}
