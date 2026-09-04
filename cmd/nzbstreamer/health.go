package main

import (
	"os"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/clientapi/webui"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/httpserver"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nntpclient"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore/sqlstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation/fusemount"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
)

// healthComponents is what the health endpoint reports on: the parts that are
// separately configured and can separately be wrong. A component gates when the
// process cannot serve its filesystem without it, which is everything but the
// news servers - with all of them unreachable the library still lists and the
// cache still serves, and depooling or restarting a singleton over a provider
// outage makes each of those worse.
func healthComponents(c Config, store *sqlstore.Store, cache *diskcache.Cache, mount *fusemount.FileSystem, pool *nntpclient.Pool, service *nzbservice.Service) []webui.Component {
	return []webui.Component{
		{Name: "metadata-db", Gates: true, Health: func() webui.Status {
			nzbs, err := store.Ping()
			if err != nil {
				return webui.Status{Status: webui.StatusDown, Details: map[string]any{"path": c.Metadata.Path, "error": err.Error()}}
			}

			// A tree still being rebuilt is a library that cannot be listed in
			// full, which is what the restore gates on until it comes from the
			// database rather than from the news server
			status := webui.StatusUp
			if !service.Ready() {
				status = webui.StatusDegraded
			}

			return webui.Status{Status: status, Details: map[string]any{
				"path":     c.Metadata.Path,
				"nzbs":     nzbs,
				"building": service.Restoring(),
			}}
		}},

		{Name: "cache", Gates: true, Health: func() webui.Status {
			items, bytes, maxBytes := cache.Stats()
			details := map[string]any{
				"path":      c.Cache.Path,
				"items":     items,
				"bytes":     bytes,
				"max_bytes": maxBytes,
			}

			// Every read stores its segment before serving it, so a cache
			// directory that went away is every read failing
			info, err := os.Stat(c.Cache.Path)
			if err != nil || !info.IsDir() {
				details["error"] = "cache directory is not there"
				return webui.Status{Status: webui.StatusDown, Details: details}
			}

			return webui.Status{Status: webui.StatusUp, Details: details}
		}},

		{Name: "mount", Gates: true, Health: func() webui.Status {
			if c.Mount.Path == "" {
				return webui.Status{Status: webui.StatusDisabled}
			}
			if !mount.Mounted() {
				return webui.Status{Status: webui.StatusDown, Details: map[string]any{"path": c.Mount.Path}}
			}

			return webui.Status{Status: webui.StatusUp, Details: map[string]any{"path": c.Mount.Path}}
		}},

		// Webdav has nothing that can be wrong with it that a body arriving does
		// not answer; it is here as the counterpart to the mount, so both
		// interfaces are read in one place rather than one of them inferred
		{Name: "webdav", Gates: false, Health: func() webui.Status {
			return webui.Status{Status: webui.StatusUp, Details: map[string]any{
				"path": httpserver.WebdavPrefix + "/",
				"auth": c.Webdav.Username != "",
			}}
		}},

		{Name: "usenet", Gates: false, Health: usenetHealth(pool)},
	}
}

// usenetHealth reports one entry per configured server and why one is out of
// rotation. Even all of them out is only degraded: an alert fires on the word,
// while the process goes on listing the library and serving what the cache
// holds.
func usenetHealth(pool *nntpclient.Pool) func() webui.Status {
	return func() webui.Status {
		servers := pool.Health()

		details := make(map[string]any, len(servers))
		up := 0
		for _, server := range servers {
			entry := map[string]any{"priority": server.Priority}
			if server.Up {
				entry["status"] = webui.StatusUp
				entry["conns"] = server.Conns
				up++
			} else {
				entry["status"] = webui.StatusDown
				entry["reason"] = server.Reason
			}
			details[server.Name] = entry
		}

		status := webui.StatusUp
		if up < len(servers) {
			status = webui.StatusDegraded
		}

		return webui.Status{Status: status, Details: details}
	}
}
