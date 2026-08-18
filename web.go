package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"main/utils/ampapi"
	"main/utils/httputil"
	"main/utils/structs"
)

//go:embed frontend/dist
var frontendFiles embed.FS

type DownloadRequest struct {
	URLs             []string `json:"urls"`
	Quality          string   `json:"quality"`
	OutputPath       string   `json:"outputPath"`
	AACType          string   `json:"aacType"`
	ALACMax          int      `json:"alacMax"`
	AtmosMax         int      `json:"atmosMax"`
	ConvertFormat    string   `json:"convertFormat"`
	KeepOriginal     bool     `json:"keepOriginal"`
	EmbedCover       bool     `json:"embedCover"`
	SaveM3U8Playlist bool     `json:"saveM3U8Playlist"`
}

type ProgressReporter func(stage string, percent int)

type deliveryResponseWriter struct {
	http.ResponseWriter
	writeErr error
}

func (w *deliveryResponseWriter) Write(data []byte) (int, error) {
	written, err := w.ResponseWriter.Write(data)
	if err != nil && w.writeErr == nil {
		w.writeErr = err
	}
	return written, err
}

// Downloader is the exported boundary used by the HTTP layer. NativeDownloader
// calls the existing Go download functions directly; it never starts the CLI as
// a child process.
type Downloader interface {
	Download(context.Context, DownloadRequest, ProgressReporter) ([]AddedTrack, error)
}

type PlaylistResolver interface {
	ResolvePlaylist(context.Context, string) (ResolvedPlaylist, error)
}

type ResolvedPlaylist struct {
	Title  string
	Tracks []ResolvedPlaylistTrack
}

type ResolvedPlaylistTrack struct {
	URL      string
	Title    string
	Artist   string
	Position int
	Total    int
}

type NativeDownloader struct {
	mu         sync.Mutex
	baseConfig structs.ConfigSet
}

func NewNativeDownloader(config structs.ConfigSet) *NativeDownloader {
	return &NativeDownloader{baseConfig: config}
}

func (d *NativeDownloader) ResolvePlaylist(ctx context.Context, rawURL string) (ResolvedPlaylist, error) {
	select {
	case <-ctx.Done():
		return ResolvedPlaylist{}, ctx.Err()
	default:
	}
	storefront, playlistID := checkUrlPlaylist(rawURL)
	if storefront == "" || playlistID == "" {
		return ResolvedPlaylist{}, errors.New("invalid playlist URL")
	}
	token, err := ampapi.GetToken()
	if err != nil {
		configured := strings.TrimSpace(d.baseConfig.AuthorizationToken)
		if configured == "" || configured == "your-authorization-token" {
			return ResolvedPlaylist{}, fmt.Errorf("resolve Apple Music authorization token: %w", err)
		}
		token = strings.TrimPrefix(configured, "Bearer ")
	}
	response, err := ampapi.GetPlaylistResp(storefront, playlistID, d.baseConfig.Language, token)
	if err != nil {
		return ResolvedPlaylist{}, fmt.Errorf("resolve playlist: %w", err)
	}
	if len(response.Data) == 0 {
		return ResolvedPlaylist{}, errors.New("playlist contains no catalog data")
	}
	playlistURL, err := url.Parse(rawURL)
	if err != nil {
		return ResolvedPlaylist{}, fmt.Errorf("parse playlist URL: %w", err)
	}
	data := response.Data[0]
	total := len(data.Relationships.Tracks.Data)
	resolved := ResolvedPlaylist{Title: data.Attributes.Name, Tracks: make([]ResolvedPlaylistTrack, 0, total)}
	for index, track := range data.Relationships.Tracks.Data {
		if strings.TrimSpace(track.ID) == "" {
			continue
		}
		trackURL := *playlistURL
		query := trackURL.Query()
		query.Set("i", track.ID)
		trackURL.RawQuery = query.Encode()
		resolved.Tracks = append(resolved.Tracks, ResolvedPlaylistTrack{
			URL:      trackURL.String(),
			Title:    track.Attributes.Name,
			Artist:   track.Attributes.ArtistName,
			Position: index + 1,
			Total:    total,
		})
	}
	if len(resolved.Tracks) == 0 {
		return ResolvedPlaylist{}, errors.New("playlist contains no downloadable tracks")
	}
	return resolved, nil
}

var activeNativeProgress ProgressReporter
var activeNativeError error
var activeNativeContext context.Context

func reportNativeProgress(stage string, percent int) {
	if activeNativeProgress != nil {
		activeNativeProgress(stage, percent)
	}
}

func recordNativeError(err error) {
	if err != nil && activeNativeError == nil {
		activeNativeError = err
	}
}

func (d *NativeDownloader) Download(ctx context.Context, request DownloadRequest, progress ProgressReporter) ([]AddedTrack, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	activeNativeProgress = progress
	activeNativeError = nil
	activeNativeContext = ctx
	defer func() {
		activeNativeProgress = nil
		activeNativeError = nil
		activeNativeContext = nil
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	Config = d.baseConfig
	Config.ExitOnError = true
	if request.OutputPath != "" {
		Config.AlacSaveFolder = filepath.Join(request.OutputPath, "ALAC")
		Config.AtmosSaveFolder = filepath.Join(request.OutputPath, "Atmos")
		Config.AacSaveFolder = filepath.Join(request.OutputPath, "AAC")
		Config.MVSaveFolder = filepath.Join(request.OutputPath, "Music Videos")
	}
	if request.AACType != "" {
		Config.AacType = request.AACType
	}
	if request.ALACMax > 0 {
		Config.AlacMax = request.ALACMax
	}
	if request.AtmosMax > 0 {
		Config.AtmosMax = request.AtmosMax
	}
	Config.ConvertAfterDownload = request.ConvertFormat != "" && request.ConvertFormat != "none"
	if Config.ConvertAfterDownload {
		Config.ConvertFormat = request.ConvertFormat
	}
	Config.ConvertKeepOriginal = request.KeepOriginal
	Config.SaveLrcFile = false
	Config.EmbedLrc = false
	Config.EmbedCover = request.EmbedCover
	if request.EmbedCover {
		// The web app embeds each track's own artwork. Apple Music's artwork URL
		// performs the resize at the CDN, so the temporary image and the embedded
		// picture are consistently capped at 1080x1080 instead of retaining the
		// playlist artwork or the much larger configured CLI cover size.
		Config.DlAlbumcoverForPlaylist = true
		Config.CoverSize = "1080x1080"
		Config.CoverFormat = "jpg"
	}

	dl_atmos = request.Quality == "atmos"
	dl_aac = request.Quality == "aac"
	dl_select = false
	dl_song = false
	artist_select = true
	debug_mode = false
	print_json = false
	save_m3u8_playlist = request.SaveM3U8Playlist
	if alac_max != nil {
		*alac_max = Config.AlacMax
	}
	if atmos_max != nil {
		*atmos_max = Config.AtmosMax
	}
	if aac_type != nil {
		*aac_type = Config.AacType
	}

	counter = structs.Counter{}
	AddedTracks = nil
	okDict = make(map[string][]int)

	if err := httputilInitForWeb(); err != nil {
		return nil, err
	}
	token, err := resolveAuthorizationToken()
	if err != nil {
		return nil, fmt.Errorf("resolve Apple Music authorization token: %w", err)
	}
	if err := executeNativeURLs(ctx, request.URLs, token); err != nil {
		return append([]AddedTrack(nil), AddedTracks...), err
	}
	if activeNativeError != nil {
		return append([]AddedTrack(nil), AddedTracks...), activeNativeError
	}
	if counter.Error > 0 {
		return append([]AddedTrack(nil), AddedTracks...), fmt.Errorf("媒体处理失败：%d 个项目未能完成", counter.Error)
	}
	return append([]AddedTrack(nil), AddedTracks...), nil
}

func httputilInitForWeb() error {
	return httputil.Init(Config.Proxy)
}

func executeNativeURLs(ctx context.Context, inputURLs []string, token string) error {
	urls := append([]string(nil), inputURLs...)
	if len(urls) == 0 {
		return errors.New("at least one Apple Music URL is required")
	}

	if strings.Contains(urls[0], "/artist/") {
		artistName, artistID, err := getUrlArtistName(urls[0], token)
		if err != nil {
			return fmt.Errorf("get artist: %w", err)
		}
		Config.ArtistFolderFormat = strings.NewReplacer(
			"{UrlArtistName}", LimitString(artistName),
			"{ArtistId}", artistID,
		).Replace(Config.ArtistFolderFormat)
		albums, err := checkArtist(urls[0], token, "albums")
		if err != nil {
			return fmt.Errorf("get artist albums: %w", err)
		}
		videos, _ := checkArtist(urls[0], token, "music-videos")
		urls = append(albums, videos...)
	}

	var taskErrors []error
	for index, rawURL := range urls {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		fmt.Printf("Web queue %d of %d: %s\n", index+1, len(urls), rawURL)
		var err error
		switch {
		case strings.Contains(rawURL, "/music-video/"):
			if len(Config.MediaUserToken) <= 50 {
				err = errors.New("media-user-token is required for music videos")
				break
			}
			if _, lookErr := exec.LookPath("mp4decrypt"); lookErr != nil {
				err = errors.New("mp4decrypt is required for music videos")
				break
			}
			storefront, videoID := checkUrlMv(rawURL)
			saveDir := strings.NewReplacer(
				"{ArtistName}", "", "{UrlArtistName}", "", "{ArtistId}", "",
			).Replace(Config.ArtistFolderFormat)
			if saveDir != "" {
				saveDir = filepath.Join(Config.MVSaveFolder, forbiddenNames.ReplaceAllString(saveDir, "_"))
			} else {
				saveDir = Config.MVSaveFolder
			}
			err = mvDownloader(videoID, saveDir, token, storefront, Config.MediaUserToken, nil)

		case strings.Contains(rawURL, "/song/"):
			storefront, songID := checkUrlSong(rawURL)
			if storefront == "" || songID == "" {
				err = errors.New("invalid song URL")
			} else {
				err = ripSong(songID, token, storefront, Config.MediaUserToken)
			}

		case strings.Contains(rawURL, "/album/"):
			parsed, parseErr := url.Parse(rawURL)
			if parseErr != nil {
				err = parseErr
				break
			}
			storefront, albumID := checkUrl(rawURL)
			if storefront == "" || albumID == "" {
				err = errors.New("invalid album URL")
				break
			}
			songID := parsed.Query().Get("i")
			dl_song = songID != ""
			err = ripAlbum(albumID, token, storefront, Config.MediaUserToken, songID)
			dl_song = false

		case strings.Contains(rawURL, "/playlist/"):
			storefront, playlistID := checkUrlPlaylist(rawURL)
			if storefront == "" || playlistID == "" {
				err = errors.New("invalid playlist URL")
			} else {
				parsed, parseErr := url.Parse(rawURL)
				if parseErr != nil {
					err = parseErr
				} else {
					err = ripPlaylist(playlistID, token, storefront, Config.MediaUserToken, parsed.Query().Get("i"))
				}
			}

		case strings.Contains(rawURL, "/station/"):
			if len(Config.MediaUserToken) <= 50 {
				err = errors.New("media-user-token is required for stations")
				break
			}
			storefront, stationID := checkUrlStation(rawURL)
			err = ripStation(stationID, token, storefront, Config.MediaUserToken)

		default:
			err = errors.New("unsupported Apple Music URL")
		}

		if err != nil {
			taskErrors = append(taskErrors, fmt.Errorf("%s: %w", rawURL, err))
		}
	}
	return errors.Join(taskErrors...)
}

type WebTask struct {
	ID            string          `json:"id"`
	Request       DownloadRequest `json:"request"`
	Title         string          `json:"title,omitempty"`
	Artist        string          `json:"artist,omitempty"`
	Collection    string          `json:"collection,omitempty"`
	CollectionID  string          `json:"collectionId,omitempty"`
	BatchID       string          `json:"batchId,omitempty"`
	QueueIndex    int             `json:"queueIndex,omitempty"`
	TrackNumber   int             `json:"trackNumber,omitempty"`
	TrackTotal    int             `json:"trackTotal,omitempty"`
	Status        string          `json:"status"`
	Progress      int             `json:"progress"`
	Stage         string          `json:"stage,omitempty"`
	StageProgress int             `json:"stageProgress,omitempty"`
	Message       string          `json:"message,omitempty"`
	Files         []TaskFile      `json:"files,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
	StartedAt     *time.Time      `json:"startedAt,omitempty"`
	EndedAt       *time.Time      `json:"endedAt,omitempty"`
}

type TaskFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	RelativePath string `json:"relativePath"`
	Size         int64  `json:"size"`
	Delivered    bool   `json:"delivered"`
	Path         string `json:"-"`
}

type taskManager struct {
	mu         sync.RWMutex
	tasks      map[string]*WebTask
	controls   map[string]*taskControl
	queue      chan string
	downloader Downloader
	resolver   PlaylistResolver
	store      *queueStore
}

type taskControl struct {
	mu      sync.Mutex
	paused  bool
	resumed chan struct{}
	cancel  context.CancelFunc
}

func newTaskControl(cancel context.CancelFunc) *taskControl {
	return &taskControl{resumed: make(chan struct{}), cancel: cancel}
}

func (control *taskControl) pause() {
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.paused {
		return
	}
	control.paused = true
	control.resumed = make(chan struct{})
}

func (control *taskControl) resume() {
	control.mu.Lock()
	defer control.mu.Unlock()
	if !control.paused {
		return
	}
	control.paused = false
	close(control.resumed)
}

func (control *taskControl) wait(ctx context.Context) error {
	control.mu.Lock()
	if !control.paused {
		control.mu.Unlock()
		return ctx.Err()
	}
	resumed := control.resumed
	control.mu.Unlock()
	select {
	case <-resumed:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (control *taskControl) cancelDownload() {
	control.mu.Lock()
	if control.paused {
		control.paused = false
		close(control.resumed)
	}
	cancel := control.cancel
	control.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func newTaskManager(downloader Downloader, resolver PlaylistResolver, store *queueStore) (*taskManager, error) {
	pending, err := store.load()
	if err != nil {
		return nil, err
	}
	queueSize := 4096
	if len(pending)+1 > queueSize {
		queueSize = len(pending) + 1
	}
	m := &taskManager{
		tasks:      make(map[string]*WebTask),
		controls:   make(map[string]*taskControl),
		queue:      make(chan string, queueSize),
		downloader: downloader,
		resolver:   resolver,
		store:      store,
	}
	for _, task := range pending {
		m.tasks[task.ID] = task
		m.queue <- task.ID
	}
	go m.worker()
	return m, nil
}

func (m *taskManager) create(request DownloadRequest) ([]*WebTask, error) {
	if strings.TrimSpace(request.OutputPath) == "" {
		return nil, errors.New("请先选择下载目录")
	}
	if len(request.URLs) == 0 {
		return nil, errors.New("urls cannot be empty")
	}
	if err := validateEmbeddingCompatibility(request); err != nil {
		return nil, err
	}
	cleaned := make([]string, 0, len(request.URLs))
	for _, rawURL := range request.URLs {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			continue
		}
		parsed, err := url.ParseRequestURI(rawURL)
		if err != nil || parsed.Scheme != "https" || !strings.HasSuffix(strings.ToLower(parsed.Hostname()), "music.apple.com") {
			return nil, fmt.Errorf("invalid Apple Music URL: %s", rawURL)
		}
		cleaned = append(cleaned, rawURL)
	}
	if len(cleaned) == 0 {
		return nil, errors.New("urls cannot be empty")
	}
	request.URLs = cleaned
	// The browser chooses the destination directory. Never accept a host or
	// container path from the client; every task downloads into isolated staging.
	request.OutputPath = ""
	if request.Quality == "" {
		request.Quality = "alac"
	}

	tasks, err := expandDownloadTasks(context.Background(), request, m.resolver)
	if err != nil {
		return nil, err
	}
	if err := m.store.enqueueMany(tasks); err != nil {
		return nil, err
	}
	m.mu.Lock()
	for _, task := range tasks {
		m.tasks[task.ID] = task
	}
	m.mu.Unlock()
	for _, task := range tasks {
		m.queue <- task.ID
	}
	result := make([]*WebTask, 0, len(tasks))
	for _, task := range tasks {
		result = append(result, cloneTask(task))
	}
	return result, nil
}

func expandDownloadTasks(ctx context.Context, request DownloadRequest, resolver PlaylistResolver) ([]*WebTask, error) {
	createdAt := time.Now().UTC()
	batchID := randomID()
	tasks := make([]*WebTask, 0, len(request.URLs))
	for _, rawURL := range request.URLs {
		if !strings.Contains(rawURL, "/playlist/") {
			taskRequest := request
			taskRequest.URLs = []string{rawURL}
			task := newQueuedTask(taskRequest, createdAt)
			task.BatchID = batchID
			task.QueueIndex = len(tasks) + 1
			tasks = append(tasks, task)
			continue
		}
		if resolver == nil {
			return nil, errors.New("playlist expansion is unavailable")
		}
		playlist, err := resolver.ResolvePlaylist(ctx, rawURL)
		if err != nil {
			return nil, err
		}
		collectionID := randomID()
		for _, track := range playlist.Tracks {
			trackRequest := request
			trackRequest.URLs = []string{track.URL}
			task := newQueuedTask(trackRequest, createdAt)
			task.Title = track.Title
			task.Artist = track.Artist
			task.Collection = playlist.Title
			task.CollectionID = collectionID
			task.BatchID = batchID
			task.QueueIndex = len(tasks) + 1
			task.TrackNumber = track.Position
			task.TrackTotal = track.Total
			tasks = append(tasks, task)
		}
	}
	if len(tasks) == 0 {
		return nil, errors.New("no downloadable tasks were resolved")
	}
	return tasks, nil
}

func newQueuedTask(request DownloadRequest, createdAt time.Time) *WebTask {
	return &WebTask{
		ID:        randomID(),
		Request:   request,
		Status:    "queued",
		Progress:  0,
		CreatedAt: createdAt,
	}
}

func (m *taskManager) worker() {
	for id := range m.queue {
		now := time.Now().UTC()
		m.mu.Lock()
		task := m.tasks[id]
		if task == nil {
			m.mu.Unlock()
			continue
		}
		if task.Status != "queued" {
			m.mu.Unlock()
			continue
		}
		request := task.Request
		if err := m.store.remove(id); err != nil {
			ended := time.Now().UTC()
			task.Status = "failed"
			task.Progress = 100
			task.Message = err.Error()
			task.EndedAt = &ended
			m.mu.Unlock()
			continue
		}
		task.Status = "running"
		task.Progress = 0
		task.Stage = "download"
		task.StageProgress = 0
		task.Message = "下载中 (0%)"
		task.StartedAt = &now
		downloadContext, cancelDownload := context.WithCancel(context.Background())
		control := newTaskControl(cancelDownload)
		m.controls[id] = control
		stagingDirectory := filepath.Join(downloadStagingRoot(), id)
		request.OutputPath = stagingDirectory
		m.mu.Unlock()

		_ = os.RemoveAll(stagingDirectory)
		if mkdirErr := os.MkdirAll(stagingDirectory, 0o700); mkdirErr != nil {
			m.finish(id, nil, fmt.Errorf("创建下载暂存目录: %w", mkdirErr))
			continue
		}
		_, err := m.downloader.Download(downloadContext, request, func(stage string, percent int) {
			if control.wait(downloadContext) != nil {
				return
			}
			m.updateProgress(id, stage, percent)
		})
		cancelDownload()
		var files []TaskFile
		if err == nil {
			// Artwork is downloaded only as a temporary source for media tags. The
			// browser should receive the tagged media file, never a cover sidecar.
			err = removeStandaloneArtwork(stagingDirectory)
		}
		if err == nil {
			files, err = collectTaskFiles(stagingDirectory)
			if err == nil && !containsMediaFile(files) {
				err = errors.New("解密失败：没有生成音频或视频文件")
			}
		} else {
			_ = os.RemoveAll(stagingDirectory)
		}
		if err != nil {
			_ = os.RemoveAll(stagingDirectory)
		}
		m.finish(id, files, err)
	}
}

func validateEmbeddingCompatibility(request DownloadRequest) error {
	format := strings.ToLower(strings.TrimSpace(request.ConvertFormat))
	if format == "" || format == "none" || format == "copy" || format == "flac" || format == "mp3" {
		return nil
	}
	if format == "opus" && request.EmbedCover {
		return errors.New("Opus 不支持可靠地内嵌封面，请关闭内嵌封面或选择 FLAC/MP3")
	}
	if format == "wav" {
		if request.EmbedCover {
			return errors.New("WAV 不支持可靠地内嵌封面，请关闭内嵌封面或选择 FLAC/MP3")
		}
	}
	return nil
}

func (m *taskManager) updateProgress(id, stage string, percent int) {
	if percent < 0 {
		percent = 0
	} else if percent > 100 {
		percent = 100
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task := m.tasks[id]
	if task == nil || task.Status != "running" {
		return
	}
	task.Stage = stage
	task.StageProgress = percent
	switch stage {
	case "decrypt":
		task.Progress = 50 + percent/2
		task.Message = fmt.Sprintf("解密中 (%d%%)", percent)
	default:
		task.Progress = percent / 2
		task.Message = fmt.Sprintf("下载中 (%d%%)", percent)
	}
}

func (m *taskManager) finish(id string, files []TaskFile, err error) {
	ended := time.Now().UTC()
	m.mu.Lock()
	task := m.tasks[id]
	if task == nil {
		m.mu.Unlock()
		return
	}
	task.EndedAt = &ended
	task.Files = files
	delete(m.controls, id)
	if task.Status == "canceling" || errors.Is(err, context.Canceled) {
		delete(m.tasks, id)
		m.mu.Unlock()
		if removeErr := m.store.remove(id); removeErr != nil {
			log.Printf("remove canceled task %s from queue database: %v", id, removeErr)
		}
		return
	} else if err != nil {
		task.Status = "failed"
		task.Stage = "failed"
		task.Message = err.Error()
	} else {
		task.Status = "completed"
		task.Stage = "completed"
		task.StageProgress = 100
		task.Progress = 100
		task.Message = ""
	}
	m.mu.Unlock()
}

func (m *taskManager) pause(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task := m.tasks[id]
	if task == nil {
		return errors.New("task not found")
	}
	if task.Status == "paused" {
		return nil
	}
	if task.Status != "running" {
		return errors.New("只有正在下载或解密的任务可以暂停")
	}
	control := m.controls[id]
	if control == nil {
		return errors.New("任务暂时无法暂停")
	}
	control.pause()
	task.Status = "paused"
	task.Message = pausedTaskMessage(task)
	return nil
}

func (m *taskManager) resume(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task := m.tasks[id]
	if task == nil {
		return errors.New("task not found")
	}
	if task.Status == "running" {
		return nil
	}
	if task.Status != "paused" {
		return errors.New("只有已暂停的任务可以继续")
	}
	control := m.controls[id]
	if control == nil {
		return errors.New("任务暂时无法继续")
	}
	task.Status = "running"
	switch task.Stage {
	case "decrypt":
		task.Message = fmt.Sprintf("解密中 (%d%%)", task.StageProgress)
	default:
		task.Message = fmt.Sprintf("下载中 (%d%%)", task.StageProgress)
	}
	control.resume()
	return nil
}

func (m *taskManager) cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task := m.tasks[id]
	if task == nil {
		return errors.New("task not found")
	}
	if task.Status == "canceled" || task.Status == "canceling" {
		return nil
	}
	if task.Status != "running" && task.Status != "paused" {
		return errors.New("只有正在下载、解密或已暂停的任务可以取消")
	}
	control := m.controls[id]
	if control == nil {
		return errors.New("任务暂时无法取消")
	}
	task.Status = "canceling"
	task.Message = "正在取消"
	control.cancelDownload()
	return nil
}

func (m *taskManager) cancelAll() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	canceled := 0
	for id, task := range m.tasks {
		switch task.Status {
		case "queued":
			if err := m.store.remove(id); err != nil {
				return canceled, err
			}
			delete(m.tasks, id)
			canceled++
		case "running", "paused":
			control := m.controls[id]
			if control == nil {
				return canceled, fmt.Errorf("任务 %s 暂时无法取消", id)
			}
			task.Status = "canceling"
			task.Message = "正在取消"
			control.cancelDownload()
			canceled++
		}
	}
	return canceled, nil
}

func pausedTaskMessage(task *WebTask) string {
	switch task.Stage {
	case "decrypt":
		return fmt.Sprintf("已暂停 · 解密中 (%d%%)", task.StageProgress)
	default:
		return fmt.Sprintf("已暂停 · 下载中 (%d%%)", task.StageProgress)
	}
}

func containsMediaFile(files []TaskFile) bool {
	for _, file := range files {
		switch strings.ToLower(filepath.Ext(file.Name)) {
		case ".m4a", ".mp4", ".m4v", ".mov", ".flac", ".mp3", ".opus", ".wav", ".aac", ".alac", ".ec3", ".ac3":
			return true
		}
	}
	return false
}

func (m *taskManager) list() []*WebTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]*WebTask, 0, len(m.tasks))
	for _, task := range m.tasks {
		items = append(items, cloneTask(task))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].BatchID != "" && items[i].BatchID == items[j].BatchID {
			return items[i].QueueIndex < items[j].QueueIndex
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	return items
}

func (m *taskManager) get(id string) (*WebTask, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[id]
	if !ok {
		return nil, false
	}
	return cloneTask(task), true
}

func cloneTask(task *WebTask) *WebTask {
	copy := *task
	copy.Request.URLs = append([]string(nil), task.Request.URLs...)
	copy.Files = append([]TaskFile(nil), task.Files...)
	return &copy
}

func downloadStagingRoot() string {
	if root := strings.TrimSpace(os.Getenv("AMDL_DOWNLOAD_STAGING")); root != "" {
		return root
	}
	return filepath.Join(os.TempDir(), "amdl-downloads")
}

func collectTaskFiles(root string) ([]TaskFile, error) {
	var files []TaskFile
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relativePath, err := filepath.Rel(root, path)
		if err != nil || relativePath == "." || filepath.IsAbs(relativePath) || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
			return fmt.Errorf("下载文件超出暂存目录: %s", path)
		}
		files = append(files, TaskFile{
			ID:           randomID(),
			Name:         filepath.Base(path),
			RelativePath: filepath.ToSlash(relativePath),
			Size:         info.Size(),
			Path:         path,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelativePath < files[j].RelativePath })
	return files, nil
}

func removeStandaloneArtwork(root string) error {
	imageExtensions := map[string]bool{
		".avif": true, ".gif": true, ".jpeg": true, ".jpg": true,
		".png": true, ".webp": true,
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !imageExtensions[strings.ToLower(filepath.Ext(entry.Name()))] {
			return nil
		}
		return os.Remove(path)
	})
}

func (m *taskManager) openTaskFile(taskID, fileID string) (*os.File, TaskFile, error) {
	m.mu.RLock()
	task := m.tasks[taskID]
	if task == nil {
		m.mu.RUnlock()
		return nil, TaskFile{}, os.ErrNotExist
	}
	var selected TaskFile
	found := false
	for _, file := range task.Files {
		if file.ID == fileID {
			selected = file
			found = true
			break
		}
	}
	m.mu.RUnlock()
	if !found || selected.Delivered || selected.Path == "" {
		return nil, TaskFile{}, os.ErrNotExist
	}
	file, err := os.Open(selected.Path)
	if err != nil {
		return nil, TaskFile{}, err
	}
	return file, selected, nil
}

func (m *taskManager) markTaskFileDelivered(taskID, fileID string) error {
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil {
		m.mu.Unlock()
		return os.ErrNotExist
	}
	for index := range task.Files {
		file := &task.Files[index]
		if file.ID != fileID {
			continue
		}
		path := file.Path
		if path == "" || file.Delivered {
			m.mu.Unlock()
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.mu.Unlock()
			return err
		}
		file.Delivered = true
		file.Path = ""
		m.mu.Unlock()
		removeEmptyStagingParents(filepath.Dir(path), filepath.Join(downloadStagingRoot(), taskID))
		return nil
	}
	m.mu.Unlock()
	return os.ErrNotExist
}

func removeEmptyStagingParents(directory, root string) {
	root = filepath.Clean(root)
	for directory = filepath.Clean(directory); directory == root || strings.HasPrefix(directory, root+string(filepath.Separator)); directory = filepath.Dir(directory) {
		if err := os.Remove(directory); err != nil {
			return
		}
		if directory == root {
			return
		}
	}
}

func randomID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

type wrapperTwoFARequest struct {
	Code string `json:"code"`
}

func wrapperFilesDirectory() string {
	if path := strings.TrimSpace(os.Getenv("AMDL_WRAPPER_FILES_DIR")); path != "" {
		return path
	}
	return "/opt/wrapper/rootfs/data/data/com.apple.android.music/files"
}

func normalizeTwoFACode(raw string) (string, error) {
	code := strings.TrimSpace(raw)
	if len(code) != 6 {
		return "", errors.New("验证码必须是 6 位数字")
	}
	for _, character := range code {
		if character < '0' || character > '9' {
			return "", errors.New("验证码必须是 6 位数字")
		}
	}
	return code, nil
}

func submitWrapperTwoFA(raw string) error {
	code, err := normalizeTwoFACode(raw)
	if err != nil {
		return err
	}
	directory := wrapperFilesDirectory()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("创建 Wrapper 验证码目录: %w", err)
	}
	codePath := filepath.Join(directory, "2fa.txt")
	temporaryPath := filepath.Join(directory, ".2fa.txt.tmp")
	if err := os.WriteFile(temporaryPath, []byte(code), 0o600); err != nil {
		return fmt.Errorf("写入 Wrapper 验证码: %w", err)
	}
	if err := os.Rename(temporaryPath, codePath); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("提交 Wrapper 验证码: %w", err)
	}

	// Wrapper waits for this file for at most 60 seconds. Remove it afterwards
	// if it still contains this code, without deleting a newer submission.
	go func(submittedCode string) {
		timer := time.NewTimer(65 * time.Second)
		defer timer.Stop()
		<-timer.C
		current, readErr := os.ReadFile(codePath)
		if readErr == nil && subtle.ConstantTimeCompare(current, []byte(submittedCode)) == 1 {
			_ = os.Remove(codePath)
		}
	}(code)
	return nil
}

func startWebServer(address string) error {
	store, err := openQueueStore(queueDatabasePath())
	if err != nil {
		return err
	}
	downloader := NewNativeDownloader(Config)
	manager, err := newTaskManager(downloader, downloader, store)
	if err != nil {
		return err
	}
	wrapper := newWrapperManager()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok", "arch": runtime.GOARCH, "os": runtime.GOOS,
			"version": "web-preview", "queueMode": "serial", "queuePersistence": "sqlite-pending-only",
		})
	})
	mux.HandleFunc("GET /api/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"qualities":      []string{"alac", "atmos", "aac"},
			"aacTypes":       []string{"aac-lc", "aac", "aac-binaural", "aac-downmix"},
			"convertFormats": []string{"none", "flac", "mp3", "opus", "wav", "copy"},
			"alacMax":        Config.AlacMax, "atmosMax": Config.AtmosMax,
		})
	})
	mux.HandleFunc("GET /api/dependencies", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, dependencyStatus())
	})
	mux.HandleFunc("GET /api/wrapper/auth", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, wrapper.status())
	})
	mux.HandleFunc("POST /api/wrapper/login", func(w http.ResponseWriter, r *http.Request) {
		var request wrapperLoginRequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := wrapper.login(request.Username, request.Password); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusAccepted, wrapper.status())
	})
	mux.HandleFunc("POST /api/wrapper/2fa", func(w http.ResponseWriter, r *http.Request) {
		var request wrapperTwoFARequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := wrapper.submitTwoFA(request.Code); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusAccepted, wrapper.status())
	})
	mux.HandleFunc("POST /api/wrapper/logout", func(w http.ResponseWriter, _ *http.Request) {
		if err := wrapper.logout(); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, wrapper.status())
	})
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"outputPath":                   Config.AlacSaveFolder,
			"authorizationTokenConfigured": Config.AuthorizationToken != "" && Config.AuthorizationToken != "your-authorization-token",
			"proxyConfigured":              Config.Proxy != "",
			"language":                     Config.Language,
		})
	})
	mux.HandleFunc("GET /api/tasks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, manager.list())
	})
	mux.HandleFunc("POST /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		if !wrapper.ready() {
			writeError(w, http.StatusServiceUnavailable, errors.New("请先完成 Wrapper 登录"))
			return
		}
		var request DownloadRequest
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		tasks, err := manager.create(request)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"tasks": tasks})
	})
	mux.HandleFunc("POST /api/tasks/cancel-all", func(w http.ResponseWriter, _ *http.Request) {
		count, err := manager.cancelAll()
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"canceled": count})
	})
	mux.HandleFunc("GET /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		task, ok := manager.get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("task not found"))
			return
		}
		writeJSON(w, http.StatusOK, task)
	})
	mux.HandleFunc("POST /api/tasks/{id}/pause", func(w http.ResponseWriter, r *http.Request) {
		if err := manager.pause(r.PathValue("id")); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		task, _ := manager.get(r.PathValue("id"))
		writeJSON(w, http.StatusOK, task)
	})
	mux.HandleFunc("POST /api/tasks/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		if err := manager.resume(r.PathValue("id")); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		task, _ := manager.get(r.PathValue("id"))
		writeJSON(w, http.StatusOK, task)
	})
	mux.HandleFunc("POST /api/tasks/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := manager.cancel(r.PathValue("id")); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		task, _ := manager.get(r.PathValue("id"))
		writeJSON(w, http.StatusOK, task)
	})
	mux.HandleFunc("GET /api/tasks/{id}/files/{fileID}", func(w http.ResponseWriter, r *http.Request) {
		file, metadata, err := manager.openTaskFile(r.PathValue("id"), r.PathValue("fileID"))
		if err != nil {
			writeError(w, http.StatusNotFound, errors.New("download file not found"))
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		disposition := mime.FormatMediaType("attachment", map[string]string{"filename": metadata.Name})
		w.Header().Set("Content-Disposition", disposition)
		w.Header().Set("Cache-Control", "no-store")
		browserDelivery := r.URL.Query().Get("delivery") == "browser"
		if !browserDelivery {
			http.ServeContent(w, r, metadata.Name, info.ModTime(), file)
			return
		}

		// Native browser downloads must transfer the entire file before its
		// container-side staging copy is removed.
		r.Header.Del("Range")
		r.Header.Del("If-Modified-Since")
		r.Header.Del("If-None-Match")
		tracked := &deliveryResponseWriter{ResponseWriter: w}
		http.ServeContent(tracked, r, metadata.Name, info.ModTime(), file)
		if tracked.writeErr != nil {
			log.Printf("Browser download interrupted for %s: %v", metadata.Name, tracked.writeErr)
			return
		}
		if err := manager.markTaskFileDelivered(r.PathValue("id"), r.PathValue("fileID")); err != nil {
			log.Printf("Could not clear browser-delivered file %s: %v", metadata.Name, err)
		}
	})
	mux.HandleFunc("DELETE /api/tasks/{id}/files/{fileID}", func(w http.ResponseWriter, r *http.Request) {
		if err := manager.markTaskFileDelivered(r.PathValue("id"), r.PathValue("fileID")); err != nil {
			writeError(w, http.StatusNotFound, errors.New("download file not found"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	})

	staticRoot, err := fs.Sub(frontendFiles, "frontend/dist")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(staticRoot)))

	server := &http.Server{
		Addr:              address,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("Apple Music Downloader web UI: http://%s", address)
	return server.ListenAndServe()
}

func dependencyStatus() map[string]any {
	tools := []string{"MP4Box", "gpac", "ffmpeg", "ffprobe", "mp4decrypt", "wrapper"}
	result := make(map[string]any, len(tools))
	for _, name := range tools {
		path, err := exec.LookPath(name)
		result[name] = map[string]any{"available": err == nil, "path": path}
	}
	return result
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
