package main

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/alphagov/router/handlers"
	"github.com/alphagov/router/logger"
	"github.com/alphagov/router/triemux"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

// Router is a wrapper around an HTTP multiplexer (trie.Mux) which retrieves its
// routes from a passed mongo database.
type Router struct {
	mux                   *triemux.Mux
	lock                  sync.RWMutex
	mongoURL              string
	mongoDbName           string
	backendConnectTimeout time.Duration
	backendHeaderTimeout  time.Duration
	logger                logger.Logger
}

type Backend struct {
	BackendID  string `bson:"backend_id"`
	BackendURL string `bson:"backend_url"`
}

type Route struct {
	Path         string `bson:"path"`
	Type         string `bson:"type"`
	Destination  string `bson:"destination"`
	SegmentsMode string `bson:"segments_mode"`
	RedirectType string `bson:"redirect_type"`
	Disabled     bool   `bson:"disabled"`
}

type ContentItem struct {
	RenderingApp string  `bson:"rendering_app"`
	DocumentType string  `bson:"document_type"`
	Routes       []Route `bson:routes`
	Redirects    []Route `bson:redirects`
}

// NewRouter returns a new empty router instance. You will still need to call
// ReloadRoutes() to do the initial route load.
func NewRouter(mongoURL, mongoDbName, backendConnectTimeout, backendHeaderTimeout, logFileName string) (rt *Router, err error) {
	beConnTimeout, err := time.ParseDuration(backendConnectTimeout)
	if err != nil {
		return nil, err
	}
	beHeaderTimeout, err := time.ParseDuration(backendHeaderTimeout)
	if err != nil {
		return nil, err
	}
	logInfo("router: using backend connect timeout:", beConnTimeout)
	logInfo("router: using backend header timeout:", beHeaderTimeout)

	l, err := logger.New(logFileName)
	if err != nil {
		return nil, err
	}
	logInfo("router: logging errors as JSON to", logFileName)

	rt = &Router{
		mux:                   triemux.NewMux(),
		mongoURL:              mongoURL,
		mongoDbName:           mongoDbName,
		backendConnectTimeout: beConnTimeout,
		backendHeaderTimeout:  beHeaderTimeout,
		logger:                l,
	}
	return rt, nil
}

// ServeHTTP delegates responsibility for serving requests to the proxy mux
// instance for this router.
func (rt *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	defer func() {
		if r := recover(); r != nil {
			logWarn("router: recovered from panic in ServeHTTP:", r)
			rt.logger.LogFromClientRequest(map[string]interface{}{"error": fmt.Sprintf("panic: %v", r), "status": 500}, req)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()
	rt.lock.RLock()
	mux := rt.mux
	rt.lock.RUnlock()

	mux.ServeHTTP(w, req)
}

// ReloadRoutes reloads the routes for this Router instance on the fly. It will
// create a new proxy mux, load applications (backends) and routes into it, and
// then flip the "mux" pointer in the Router.
func (rt *Router) ReloadRoutes() {
	defer func() {
		if r := recover(); r != nil {
			logWarn("router: recovered from panic in ReloadRoutes:", r)
			logInfo("router: original routes have not been modified")
		}
	}()

	logDebug("mgo: connecting to", rt.mongoURL)
	sess, err := mgo.Dial(rt.mongoURL)
	if err != nil {
		panic(fmt.Sprintln("mgo:", err))
	}
	defer sess.Close()
	sess.SetMode(mgo.Strong, true)

	db := sess.DB(rt.mongoDbName)

	logInfo("router: reloading routes")
	newmux := triemux.NewMux()

	backends := rt.loadBackends(db.C("backends"))
	logInfo(fmt.Sprintf("router: reloaded %d backends", len(backends)))
	loadRoutes(db.C("content_items"), newmux, backends)

	rt.lock.Lock()
	rt.mux = newmux
	rt.lock.Unlock()

	logInfo(fmt.Sprintf("router: reloaded %d routes (checksum: %x)", rt.mux.RouteCount(), rt.mux.RouteChecksum()))
}

// loadBackends is a helper function which loads backends from the
// passed mongo collection, constructs a Handler for each one, and returns
// them in map keyed on the backend_id
func (rt *Router) loadBackends(c *mgo.Collection) (backends map[string]http.Handler) {
	backend := &Backend{}
	backends = make(map[string]http.Handler)

	iter := c.Find(nil).Iter()

	for iter.Next(&backend) {
		backendURL, err := url.Parse(backend.BackendURL)
		if err != nil {
			logWarn(fmt.Sprintf("router: couldn't parse URL %s for backend %s "+
				"(error: %v), skipping!", backend.BackendURL, backend.BackendID, err))
			continue
		}

		backends[backend.BackendID] = handlers.NewBackendHandler(backendURL, rt.backendConnectTimeout, rt.backendHeaderTimeout, rt.logger)
	}
	backends["gone"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "410 gone", http.StatusGone)
	})
	backends["unavailable"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "503 Service Unavailable", http.StatusServiceUnavailable)
	})

	if err := iter.Err(); err != nil {
		panic(err)
	}

	return
}

func loadRoute(route *Route, documentType string, renderingApp string, mux *triemux.Mux, backends map[string]http.Handler) {

	prefix := (route.Type == "prefix")

	// the database contains paths with % encoded routes.
	// Unescape them here because the http.Request objects we match against contain the unescaped variants.
	incomingURL, err := url.Parse(route.Path)
	if err != nil {
		logWarn(fmt.Sprintf("router: found route %+v with invalid path '%s', skipping!", route, route.Path))
		return
	}

	if route.Disabled {
		mux.Handle(incomingURL.Path, prefix, backends["unavailable"])
		logDebug(fmt.Sprintf("router: registered %s (prefix: %v)(disabled) -> Unavailable", incomingURL.Path, prefix))
		return
	}

	switch documentType {
	case "boom":
		// Special handler so that we can test failure behaviour.
		mux.Handle(incomingURL.Path, prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("Boom!!!")
		}))
		logDebug(fmt.Sprintf("router: registered %s (prefix: %v) -> Boom!!!", incomingURL.Path, prefix))
	case "gone":
		mux.Handle(incomingURL.Path, prefix, backends["gone"])
		logDebug(fmt.Sprintf("router: registered %s (prefix: %v) -> Gone", incomingURL.Path, prefix))
	case "redirect":
		redirectTemporarily := (route.RedirectType == "temporary")
		handler := handlers.NewRedirectHandler(incomingURL.Path, route.Destination, shouldPreserveSegments(route), redirectTemporarily)
		mux.Handle(incomingURL.Path, prefix, handler)
		logDebug(fmt.Sprintf("router: registered %s (prefix: %v) -> %s",
			incomingURL.Path, prefix, route.Destination))
	default:
		handler, ok := backends[renderingApp]
		if !ok {
			logWarn(fmt.Sprintf("router: found route %+v which references unknown rendering app "+
				"%s, skipping!", route, renderingApp))
			return
		}
		mux.Handle(incomingURL.Path, prefix, handler)
		logDebug(fmt.Sprintf("router: registered %s (prefix: %v) for %s",
			incomingURL.Path, prefix, renderingApp))
	}
}

// loadRoutes is a helper function which loads routes from the passed mongo
// collection and registers them with the passed proxy mux.
func loadRoutes(c *mgo.Collection, mux *triemux.Mux, backends map[string]http.Handler) {
	contentItem := &ContentItem{}

	iter := c.Find(nil).Select(bson.M{"rendering_app": 1, "document_type": 1, "redirects": 1, "routes": 1}).Iter()

	for iter.Next(&contentItem) {
		for _, route := range contentItem.Routes {
			loadRoute(&route, contentItem.DocumentType, contentItem.RenderingApp, mux, backends)
		}

		for _, redirect := range contentItem.Redirects {
			loadRoute(&redirect, "redirect", contentItem.RenderingApp, mux, backends)
		}
	}

	if err := iter.Err(); err != nil {
		panic(err)
	}
}

func (rt *Router) RouteStats() (stats map[string]interface{}) {
	rt.lock.RLock()
	mux := rt.mux
	rt.lock.RUnlock()

	stats = make(map[string]interface{})
	stats["count"] = mux.RouteCount()
	stats["checksum"] = fmt.Sprintf("%x", mux.RouteChecksum())
	return
}

func shouldPreserveSegments(redirect *Route) bool {
	switch {
	case redirect.Type == "exact" && redirect.SegmentsMode == "preserve":
		return true
	case redirect.Type == "exact":
		return false
	case redirect.Type == "prefix" && redirect.SegmentsMode == "ignore":
		return false
	case redirect.Type == "prefix":
		return true
	}
	return false
}
