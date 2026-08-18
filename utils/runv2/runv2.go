package runv2

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"main/utils/structs"

	"github.com/grafov/m3u8"
	"github.com/itouakirai/mp4ff/mp4"
	"github.com/schollz/progressbar/v3"
)

const prefetchKey = "skd://itunes.apple.com/P000000000/s1/e1"

var ErrTimeout = errors.New("response timed out")

type TimedResponseBody struct {
	timeout   time.Duration
	timer     *time.Timer
	threshold int
	body      io.Reader
}

type callbackWriter struct {
	total   int64
	written int64
	last    int
	stage   string
	report  func(string, int)
}

func (w *callbackWriter) Write(p []byte) (int, error) {
	w.written += int64(len(p))
	percent := 0
	if w.total > 0 {
		percent = int(w.written * 100 / w.total)
		if percent > 100 {
			percent = 100
		}
	}
	if percent != w.last {
		w.last = percent
		w.report(w.stage, percent)
	}
	return len(p), nil
}

func emitProgress(report func(string, int), stage string, percent int) {
	if report != nil {
		report(stage, percent)
	}
}

func (b *TimedResponseBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil {
		return n, err
	}
	// fmt.Printf("Read %d bytes, buffer size %d bytes", n, len(p))
	if n >= b.threshold {
		b.timer.Reset(b.timeout)
	}
	return n, err
}

func Run(parentContext context.Context, adamId string, playlistUrl string, outfile string, Config structs.ConfigSet, progress func(string, int)) error {
	if parentContext == nil {
		parentContext = context.Background()
	}
	emitProgress(progress, "download", 0)
	var err error
	var optstimeout uint
	optstimeout = 0
	timeout := time.Duration(optstimeout * uint(time.Millisecond))
	header := make(http.Header)

	// request media playlist
	req, err := http.NewRequestWithContext(parentContext, "GET", playlistUrl, nil)
	if err != nil {
		return err
	}
	req.Header = header
	// requesting an HLS playlist should be relatively fast, so we set the timeout directly on the client
	do, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return err
	}

	// parse m3u8
	segments, err := parseMediaPlaylist(do.Body)
	if err != nil {
		return err
	}
	segment := segments[0]
	if segment == nil {
		return errors.New("no segments extracted from playlist")
	}
	if segment.Limit <= 0 {
		return errors.New("non-byterange playlists are currently unsupported")
	}

	// get URL to the actual file
	parsedUrl, err := url.Parse(playlistUrl)
	if err != nil {
		return err
	}
	fileUrl, err := parsedUrl.Parse(segment.URI)
	if err != nil {
		return err
	}

	// request mp4
	ctx, cancel := context.WithCancelCause(parentContext)
	defer cancel(nil)
	req, err = http.NewRequestWithContext(ctx, "GET", fileUrl.String(), nil)
	if err != nil {
		return err
	}
	req.Header = header

	var body io.Reader
	var bufferedMedia []byte
	client := &http.Client{Timeout: timeout}
	if optstimeout > 0 {
		// create the timer before calling Do so that the timeout covers TCP handshake,
		// TLS handshake, sending the request and receiving HTTP headers
		timer := time.AfterFunc(timeout, func() { cancel(ErrTimeout) })
		do, err = client.Do(req)
		if err != nil {
			return err
		}
		defer do.Body.Close()
		body = &TimedResponseBody{
			timeout:   timeout,
			timer:     timer,
			threshold: 256,
			body:      do.Body,
		}
	} else {
		do, err = client.Do(req)
		if err != nil {
			return err
		}
		defer do.Body.Close()
		if do.ContentLength < int64(Config.MaxMemoryLimit*1024*1024) {
			var buffer bytes.Buffer
			bar := progressbar.NewOptions64(
				do.ContentLength,
				progressbar.OptionClearOnFinish(),
				progressbar.OptionSetElapsedTime(false),
				progressbar.OptionSetPredictTime(false),
				progressbar.OptionShowElapsedTimeOnFinish(),
				progressbar.OptionShowCount(),
				progressbar.OptionEnableColorCodes(true),
				progressbar.OptionShowBytes(true),
				progressbar.OptionSetDescription("Downloading..."),
				progressbar.OptionSetTheme(progressbar.Theme{
					Saucer:        "",
					SaucerHead:    "",
					SaucerPadding: "",
					BarStart:      "",
					BarEnd:        "",
				}),
			)
			tracker := &callbackWriter{total: do.ContentLength, stage: "download", report: progress}
			if _, err := io.Copy(io.MultiWriter(&buffer, bar, tracker), do.Body); err != nil {
				return err
			}
			emitProgress(progress, "download", 100)
			bufferedMedia = buffer.Bytes()
			body = bytes.NewReader(bufferedMedia)
			fmt.Print("Downloaded\n")
		} else {
			body = do.Body
			emitProgress(progress, "download", 100)
		}
	}

	var totalLen int64
	totalLen = do.ContentLength
	err = decryptWithWrapper(ctx, body, outfile, adamId, segments, totalLen, Config, progress)
	for retry := 1; retry <= 2 && err != nil && bufferedMedia != nil && isRetryableDecryptError(err); retry++ {
		// The current ARM64 Wrapper release can terminate its Android child while
		// establishing the first FairPlay context after login. The web process
		// restarts Wrapper automatically, so retain the encrypted response in
		// memory and retry only decryption after the socket is available. Two
		// retries cover an externally interrupted process followed by Wrapper's
		// one-time context initialization exit without masking content errors.
		fmt.Printf("Decryptor connection was interrupted; waiting for Wrapper (retry %d/2): %v\n", retry, err)
		if waitErr := waitForDecryptor(Config.DecryptM3u8Port, 12*time.Second); waitErr != nil {
			break
		}
		if refreshErr := refreshDevicePlaylist(Config.GetM3u8Port, adamId); refreshErr != nil {
			err = fmt.Errorf("restore Wrapper playback context: %w", refreshErr)
			continue
		}
		emitProgress(progress, "decrypt", 0)
		err = decryptWithWrapper(ctx, bytes.NewReader(bufferedMedia), outfile, adamId, segments, totalLen, Config, progress)
	}
	if err != nil {
		return err
	}
	fmt.Print("Decrypted\n")
	return nil
}

func decryptWithWrapper(ctx context.Context, body io.Reader, outfile string, adamId string, segments []*m3u8.MediaSegment,
	totalLen int64, Config structs.ConfigSet, progress func(string, int)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := warmDecryptor(Config.DecryptM3u8Port, adamId, segments); err != nil {
		return fmt.Errorf("prepare Wrapper key context: %w", err)
	}
	conn, err := net.DialTimeout("tcp", Config.DecryptM3u8Port, 2*time.Second)
	if err != nil {
		return err
	}
	defer Close(conn)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)
	return downloadAndDecryptFile(ctx, conn, body, outfile, adamId, segments, totalLen, Config, progress)
}

func warmDecryptor(address string, adamId string, segments []*m3u8.MediaSegment) error {
	seen := make(map[string]struct{})
	for _, segment := range segments {
		if segment == nil || segment.Key == nil || segment.Key.URI == "" {
			continue
		}
		keyAdamID := adamId
		if segment.Key.URI == prefetchKey {
			keyAdamID = "0"
		}
		identity := keyAdamID + "\x00" + segment.Key.URI
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		if err := warmDecryptorKey(address, keyAdamID, segment.Key.URI); err != nil {
			return err
		}
	}
	return nil
}

func warmDecryptorKey(address string, adamId string, keyURI string) error {
	connection, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return err
	}
	defer Close(connection)
	rw := bufio.NewReadWriter(bufio.NewReader(connection), bufio.NewWriter(connection))
	if err := SendString(rw, adamId); err != nil {
		return err
	}
	if err := SendString(rw, keyURI); err != nil {
		return err
	}
	probe := make([]byte, 16)
	if err := binary.Write(rw, binary.LittleEndian, uint32(len(probe))); err != nil {
		return err
	}
	if _, err := rw.Write(probe); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	_, err = io.ReadFull(rw, probe)
	return err
}

func refreshDevicePlaylist(address string, adamId string) error {
	if len(adamId) == 0 || len(adamId) > 255 {
		return errors.New("invalid adam ID for Wrapper m3u8 request")
	}
	connection, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return err
	}
	defer connection.Close()
	if _, err := connection.Write(append([]byte{byte(len(adamId))}, []byte(adamId)...)); err != nil {
		return err
	}
	response, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(strings.TrimSpace(response), ".m3u8") {
		return errors.New("Wrapper returned an empty device playlist")
	}
	return nil
}

func isRetryableDecryptError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection reset", "connection refused", "broken pipe", "unexpected eof", "use of closed network connection",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return errors.Is(err, io.EOF)
}

func waitForDecryptor(address string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	// Give the wrapper manager time to observe the old child exit before a
	// readiness probe can accidentally connect to its still-closing socket.
	time.Sleep(time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			// Let the manager promote the same process to authenticated before the
			// caller restores its device playback context over the m3u8 socket.
			time.Sleep(500 * time.Millisecond)
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("Wrapper did not restore %s within %s", address, timeout)
}

func downloadAndDecryptFile(ctx context.Context, conn io.ReadWriter, in io.Reader, outfile string,
	adamId string, playlistSegments []*m3u8.MediaSegment, totalLen int64, Config structs.ConfigSet, progress func(string, int)) error {
	emitProgress(progress, "decrypt", 0)
	var buffer bytes.Buffer
	var outBuf *bufio.Writer
	MaxMemorySize := int64(Config.MaxMemoryLimit * 1024 * 1024)
	inBuf := bufio.NewReader(in)
	if totalLen <= MaxMemorySize {
		outBuf = bufio.NewWriter(&buffer)
	} else {
		ofh, err := os.Create(outfile)
		if err != nil {
			return err
		}
		defer ofh.Close()
		outBuf = bufio.NewWriter(ofh)
	}
	init, offset, err := ReadInitSegment(inBuf)
	if err != nil {
		return err
	}
	if init == nil {
		return errors.New("no init segment found")
	}

	tracks, err := TransformInit(init)
	if err != nil {
		return err
	}
	err = sanitizeInit(init)
	if err != nil {
		// errors returned by sanitizeInit are non-fatal
		fmt.Printf("Warning: unable to sanitize init completely: %s\n", err)
	}
	err = init.Encode(outBuf)
	if err != nil {
		return err
	}

	// 'segment' in m3u8 == 'fragment' in mp4ff
	//fmt.Println("Starting decryption...")
	bar := progressbar.NewOptions64(totalLen,
		progressbar.OptionClearOnFinish(),
		progressbar.OptionSetElapsedTime(false),
		progressbar.OptionSetPredictTime(false),
		progressbar.OptionShowElapsedTimeOnFinish(),
		progressbar.OptionShowCount(),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetDescription("Decrypting..."),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "",
			SaucerHead:    "",
			SaucerPadding: "",
			BarStart:      "",
			BarEnd:        "",
		}),
	)
	bar.Add64(int64(offset))
	if totalLen > 0 {
		emitProgress(progress, "decrypt", int(int64(offset)*100/totalLen))
	}
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	for i := 0; ; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var frag *mp4.Fragment
		rawoffset := offset
		frag, offset, err = ReadNextFragment(inBuf, offset)
		rawoffset = offset - rawoffset
		if err != nil {
			return err
		}
		if frag == nil {
			// check offset against Content-Length?
			break
		}
		// print progress

		// if totalLen > 0 {
		// 	fmt.Printf("%.2f%% of %d bytes\n", 100*float32(offset)/float32(totalLen), totalLen)
		// }
		segment := playlistSegments[i]
		if segment == nil {
			return errors.New("segment number out of sync")
		}
		key := segment.Key
		if key != nil {
			if i != 0 {
				SwitchKeys(rw)
			}
			if key.URI == prefetchKey {
				SendString(rw, "0")
			} else {
				SendString(rw, adamId)
			}
			SendString(rw, key.URI)
		}
		// flushes the buffer
		err = DecryptFragment(frag, tracks, rw)
		if err != nil {
			return fmt.Errorf("decryptFragment: %w", err)
		}
		err = frag.Encode(outBuf)
		if err != nil {
			return err
		}
		bar.Add64(int64(rawoffset))
		if totalLen > 0 {
			emitProgress(progress, "decrypt", int(int64(offset)*100/totalLen))
		}
	}
	err = outBuf.Flush()
	if err != nil {
		return err
	}
	if totalLen <= MaxMemorySize {
		// create output file
		ofh, err := os.Create(outfile)
		if err != nil {
			return err
		}
		defer ofh.Close()

		_, err = ofh.Write(buffer.Bytes())
		if err != nil {
			return err
		}
	}
	emitProgress(progress, "decrypt", 100)
	return nil
}

// Remove boxes in the init segment that are known to cause compatibility issues
func sanitizeInit(init *mp4.InitSegment) error {
	traks := init.Moov.Traks
	if len(traks) > 1 {
		return errors.New("more than 1 track found")
	}
	// Remove duplicate ec-3 or alac boxes in stsd since some programs (e.g. cuetools) don't
	// like it when there's more than 1 entry in stsd.
	// Every audio track contains two of these boxes because two IVs are needed to decrypt the
	// track. The two boxes become identical after removing encryption info.
	stsd := traks[0].Mdia.Minf.Stbl.Stsd
	if stsd.SampleCount == 1 {
		return nil
	}
	if stsd.SampleCount > 2 {
		return fmt.Errorf("expected only 1 or 2 entries in stsd, got %d", stsd.SampleCount)
	}
	children := stsd.Children
	if children[0].Type() != children[1].Type() {
		return errors.New("children in stsd are not of the same type")
	}
	stsd.Children = children[:1]
	stsd.SampleCount = 1
	return nil
}

// Workaround for m3u8 not supporting multiple keys - remove
// PlayReady and Widevine
func filterResponse(f io.Reader) (*bytes.Buffer, error) {
	buf := &bytes.Buffer{}
	scanner := bufio.NewScanner(f)

	prefix := []byte("#EXT-X-KEY:")
	keyFormat := []byte("streamingkeydelivery")
	for scanner.Scan() {
		lineBytes := scanner.Bytes()
		if bytes.HasPrefix(lineBytes, prefix) && !bytes.Contains(lineBytes, keyFormat) {
			continue
		}
		_, err := buf.Write(lineBytes)
		if err != nil {
			return nil, err
		}
		_, err = buf.WriteString("\n")
		if err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return buf, nil
}

func parseMediaPlaylist(r io.ReadCloser) ([]*m3u8.MediaSegment, error) {
	defer r.Close()
	playlistBuf, err := filterResponse(r)
	if err != nil {
		return nil, err
	}

	playlist, listType, err := m3u8.Decode(*playlistBuf, true)
	if err != nil {
		return nil, err
	}

	if listType != m3u8.MEDIA {
		return nil, errors.New("m3u8 not of media type")
	}

	mediaPlaylist := playlist.(*m3u8.MediaPlaylist)
	return mediaPlaylist.Segments, nil
}

// pasing
func ReadInitSegment(r io.Reader) (*mp4.InitSegment, uint64, error) {
	var offset uint64 = 0
	init := mp4.NewMP4Init()
	for i := 0; i < 2; i++ {
		box, err := mp4.DecodeBox(offset, r)
		if err != nil {
			return nil, offset, err
		}
		boxType := box.Type()
		if boxType != "ftyp" && boxType != "moov" {
			return nil, offset, fmt.Errorf("unexpected box type %s, should be ftyp or moov", boxType)
		}
		init.AddChild(box)
		offset += box.Size()
	}
	return init, offset, nil
}

// Get the next fragment. Returns nil and no error on EOF
func ReadNextFragment(r io.Reader, offset uint64) (*mp4.Fragment, uint64, error) {
	frag := mp4.NewFragment()
	for {
		box, err := mp4.DecodeBox(offset, r)
		if err == io.EOF {
			return nil, offset, nil
		}
		if err != nil {
			return nil, offset, err
		}
		boxType := box.Type()
		// fmt.Printf("processing %s, box starts @ offset %d\n", boxType, offset)
		offset += box.Size()
		if boxType == "moof" || boxType == "emsg" || boxType == "prft" {
			frag.AddChild(box)
			continue
		}
		if boxType == "mdat" {
			frag.AddChild(box)
			break
		}
		fmt.Printf("ignoring a %s box found mid-stream", boxType)
	}
	// only 1 mdat box in fragment, meaning that the box doesn't have a preceding moof box
	if frag.Moof == nil {
		return nil, offset, fmt.Errorf("more than one mdat box in fragment (box ends @ offset %d)", offset)
	}
	return frag, offset, nil
}

// Return a new slice of boxes with encryption-related sbgp and sgpd removed,
// and the total number of bytes removed.
// Non-encryption-related ones such as 'roll' are left untouched.
func FilterSbgpSgpd(children []mp4.Box) ([]mp4.Box, uint64) {
	var bytesRemoved uint64 = 0
	remainingChildren := make([]mp4.Box, 0, len(children))
	for _, child := range children {
		switch box := child.(type) {
		case *mp4.SbgpBox:
			if box.GroupingType == "seam" || box.GroupingType == "seig" {
				bytesRemoved += child.Size()
				continue
			}
		case *mp4.SgpdBox:
			if box.GroupingType == "seam" || box.GroupingType == "seig" {
				bytesRemoved += child.Size()
				continue
			}
		}
		remainingChildren = append(remainingChildren, child)
	}
	return remainingChildren, bytesRemoved
}

// Get decryption info for tracks from init segment and remove encryption-related boxes
func TransformInit(init *mp4.InitSegment) (map[uint32]mp4.DecryptTrackInfo, error) {
	di, err := mp4.DecryptInit(init)
	tracks := make(map[uint32]mp4.DecryptTrackInfo, len(di.TrackInfos))
	for _, ti := range di.TrackInfos {
		tracks[ti.TrackID] = ti
	}
	if err != nil {
		return tracks, err
	}
	// remove encryption-related sbgp and sgpd
	for _, trak := range init.Moov.Traks {
		stbl := trak.Mdia.Minf.Stbl
		stbl.Children, _ = FilterSbgpSgpd(stbl.Children)
	}
	return tracks, nil
}

// remote
// Reset the loops on the script's end and close the connection
func Close(conn io.WriteCloser) error {
	defer conn.Close()
	_, err := conn.Write([]byte{0, 0, 0, 0, 0})
	return err
}

func SwitchKeys(conn io.Writer) error {
	_, err := conn.Write([]byte{0, 0, 0, 0})
	return err
}

// Send id or keyUri
func SendString(conn io.Writer, uri string) error {
	_, err := conn.Write([]byte{byte(len(uri))})
	if err != nil {
		return err
	}
	_, err = io.WriteString(conn, uri)
	return err
}

func cbcsFullSubsampleDecrypt(data []byte, conn *bufio.ReadWriter) error {
	// Drops 4 last bits -> multiple of 16
	// It wouldn't hurt to send the remaining bytes also because the decryption
	// function would just return them as-is, but we're truncating the data here
	// for clarity and interoperability
	truncatedLen := len(data) & ^0xf
	// send the whole chunk at once
	err := binary.Write(conn, binary.LittleEndian, uint32(truncatedLen))
	if err != nil {
		return err
	}
	_, err = conn.Write(data[:truncatedLen])
	if err != nil {
		return err
	}
	err = conn.Flush()
	if err != nil {
		return err
	}
	_, err = io.ReadFull(conn, data[:truncatedLen])
	return err
}

func cbcsStripeDecrypt(data []byte, conn *bufio.ReadWriter, decryptBlockLen, skipBlockLen int) error {
	size := len(data)

	// block too small, ignore
	if size < decryptBlockLen {
		return nil
	}

	// number of encrypted blocks in this sample
	count := ((size - decryptBlockLen) / (decryptBlockLen + skipBlockLen)) + 1
	totalLen := count * decryptBlockLen

	err := binary.Write(conn, binary.LittleEndian, uint32(totalLen))
	if err != nil {
		return err
	}

	pos := 0
	for {
		if size-pos < decryptBlockLen { // Leave the rest
			break
		}
		_, err = conn.Write(data[pos : pos+decryptBlockLen])
		if err != nil {
			return err
		}
		pos += decryptBlockLen
		if size-pos < skipBlockLen {
			break
		}
		pos += skipBlockLen
	}
	err = conn.Flush()
	if err != nil {
		return err
	}

	pos = 0
	for {
		if size-pos < decryptBlockLen {
			break
		}
		_, err = io.ReadFull(conn, data[pos:pos+decryptBlockLen])
		if err != nil {
			return err
		}
		pos += decryptBlockLen
		if size-pos < skipBlockLen {
			break
		}
		pos += skipBlockLen
	}
	return nil
}

// Decryption function dispatcher
func cbcsDecryptRaw(data []byte, conn *bufio.ReadWriter, decryptBlockLen, skipBlockLen int) error {
	if skipBlockLen == 0 {
		// Full encryption of subsamples
		// e.g. Apple Music ALAC
		return cbcsFullSubsampleDecrypt(data, conn)
	} else {
		// Pattern (stripe) encryption of subsamples
		// e.g. most AVC and HEVC applications
		return cbcsStripeDecrypt(data, conn, decryptBlockLen, skipBlockLen)
	}
}

// Decrypt a cbcs-encrypted sample in-place
func cbcsDecryptSample(sample []byte, conn *bufio.ReadWriter,
	subSamplePatterns []mp4.SubSamplePattern, tenc *mp4.TencBox) error {

	decryptBlockLen := int(tenc.DefaultCryptByteBlock) * 16
	skipBlockLen := int(tenc.DefaultSkipByteBlock) * 16
	var pos uint32 = 0

	// Full sample encryption
	if len(subSamplePatterns) == 0 {
		return cbcsDecryptRaw(sample, conn, decryptBlockLen, skipBlockLen)
	}

	// Has subsamples
	for j := 0; j < len(subSamplePatterns); j++ {
		ss := subSamplePatterns[j]
		pos += uint32(ss.BytesOfClearData)

		// Nothing to decrypt!
		if ss.BytesOfProtectedData <= 0 {
			continue
		}

		err := cbcsDecryptRaw(sample[pos:pos+ss.BytesOfProtectedData],
			conn, decryptBlockLen, skipBlockLen)
		if err != nil {
			return err
		}
		pos += ss.BytesOfProtectedData
	}

	return nil
}

// Decrypt an array of cbcs-encrypted samples in-place
func cbcsDecryptSamples(samples []mp4.FullSample, conn *bufio.ReadWriter,
	tenc *mp4.TencBox, senc *mp4.SencBox) error {

	for i := range samples {
		var subSamplePatterns []mp4.SubSamplePattern
		if len(senc.SubSamples) != 0 {
			subSamplePatterns = senc.SubSamples[i]
		}
		err := cbcsDecryptSample(samples[i].Data, conn, subSamplePatterns, tenc)
		if err != nil {
			return err
		}
	}
	return nil
}

func DecryptFragment(frag *mp4.Fragment, tracks map[uint32]mp4.DecryptTrackInfo, conn *bufio.ReadWriter) error {
	moof := frag.Moof
	var bytesRemoved uint64 = 0

	for _, traf := range moof.Trafs {
		ti, ok := tracks[traf.Tfhd.TrackID]
		if !ok {
			return fmt.Errorf("could not find decryption info for track %d", traf.Tfhd.TrackID)
		}
		if ti.Sinf == nil {
			// unencrypted track
			continue
		}

		schemeType := ti.Sinf.Schm.SchemeType
		if schemeType != "cbcs" {
			return fmt.Errorf("scheme type %s not supported", schemeType)
		}
		hasSenc, isParsed := traf.ContainsSencBox()
		if !hasSenc {
			return fmt.Errorf("no senc box in traf")
		}

		var senc *mp4.SencBox
		if traf.Senc != nil {
			senc = traf.Senc
		} else {
			senc = traf.UUIDSenc.Senc
		}

		if !isParsed {
			// simply ignore sbgp and sgpd
			// "Sample To Group Box ('sbgp') and Sample Group Description Box ('sgpd')
			// of type 'seig' are used to indicate the KID applied to each sample, and changes
			// to KIDs over time (i.e. 'key rotation')"
			// (ref: https://dashif.org/docs/DASH-IF-IOP-v3.2.pdf)
			err := senc.ParseReadBox(ti.Sinf.Schi.Tenc.DefaultPerSampleIVSize, traf.Saiz)
			if err != nil {
				return err
			}
		}

		samples, err := frag.GetFullSamples(ti.Trex)
		if err != nil {
			return err
		}

		err = cbcsDecryptSamples(samples, conn, ti.Sinf.Schi.Tenc, senc)
		if err != nil {
			return err
		}

		bytesRemoved += traf.RemoveEncryptionBoxes()
	}
	_, psshBytesRemoved := moof.RemovePsshs()
	bytesRemoved += psshBytesRemoved
	for _, traf := range moof.Trafs {
		for _, trun := range traf.Truns {
			trun.DataOffset -= int32(bytesRemoved)
		}
	}

	return nil
}
