package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/fatih/color"
	"github.com/schollz/progressbar/v3"

	"github.com/dhowden/tag"
	"github.com/urfave/cli/v2"
)

const (
	tolerance        = 0.01
	maxOverlapping   = 10
	maxSilence       = 2
	minSilence       = 0.6
	silenceTolerance = 0.4
	workers          = 10
)

type artist struct {
	name         string
	albums       map[string]*album
	tracksByName map[string]*track
}

type album struct {
	name         string
	tracks       map[int]map[int]*track
	tracksByName map[string]*track
	*artist
}

type track struct {
	name         string
	path         string
	trackNumber  int
	diskNumber   int
	silences     []silence
	longSilences []silence
	duration     float64
	*album
}

type missingTrack struct {
	name string
	*album
}

type wrongTrack struct {
	expectedDuration float64
	duration         float64
	*track
}

type artistMap map[string]*artist

type silence struct {
	start float64
	end   float64
}

var (
	duration     = regexp.MustCompile(`Duration: ([0-9]{2}):([0-9]{2}):([0-9\.]+), start: ([0-9\.]+), bitrate: [0-9\.]+ kb/s`)
	silenceStart = regexp.MustCompile(`\[silencedetect @ .*?\] silence_start: ([0-9\.]+)`)
	silenceEnd   = regexp.MustCompile(`\[silencedetect @ .*?\] silence_end: ([0-9\.]+) \| silence_duration: [0-9\.]+`)
)

func getSilenceInfo(path string, longSilence bool) ([]silence, float64) {
	d := 0.0
	result := []silence{}

	silenceDetectArg := ""
	if longSilence {
		silenceDetectArg = "silencedetect=d=" + fmt.Sprintf("%f", maxSilence+silenceTolerance)
	} else {
		silenceDetectArg = "silencedetect=noise=-25dB:d=" + fmt.Sprintf("%f", minSilence-silenceTolerance)
	}

	output, _ := exec.Command("ffmpeg", "-i", path, "-af", silenceDetectArg, "-vcodec", "copy", "-f", "null", "-").CombinedOutput()
	outputString := string(output)

	lines := strings.Split(strings.Replace(outputString, "\r\n", "\n", -1), "\n")

	currentSilence := silence{}
	for _, line := range lines {
		startMatch := silenceStart.FindStringSubmatch(line)
		if startMatch != nil {
			currentSilence.start, _ = strconv.ParseFloat(startMatch[1], 64)
			continue
		}

		endMatch := silenceEnd.FindStringSubmatch(line)
		if endMatch != nil {
			currentSilence.end, _ = strconv.ParseFloat(endMatch[1], 64)
			currentDuration := currentSilence.end - currentSilence.start

			if currentDuration > minSilence || currentSilence.start < tolerance || d-currentSilence.end < tolerance {
				result = append(result, currentSilence)
			}

			continue
		}

		durationMatch := duration.FindStringSubmatch(line)
		if durationMatch != nil {
			hours, _ := strconv.ParseFloat(durationMatch[1], 64)
			minutes, _ := strconv.ParseFloat(durationMatch[2], 64)
			seconds, _ := strconv.ParseFloat(durationMatch[3], 64)
			offset, _ := strconv.ParseFloat(durationMatch[4], 64)
			d = hours*3600 + minutes*60 + seconds - offset
		}
	}

	return result, d
}

func getFolderData(folder string) *artistMap {
	data := artistMap{}

	filepath.Walk(folder, func(path string, info os.FileInfo, err error) error {
		if strings.ToLower(filepath.Ext(path)) != ".mp3" {
			return nil
		}

		if err != nil {
			return err
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}

		defer file.Close()

		metadata, err := tag.ReadFrom(file)
		if err != nil {
			return err
		}

		artistName := strings.Trim(metadata.AlbumArtist(), " ")
		if artistName == "" {
			artistName = strings.Trim(strings.Split(metadata.Artist(), ",")[0], " ")
		}

		albumName := strings.Trim(metadata.Album(), " ")
		diskNumber, _ := metadata.Disc()
		trackNumber, _ := metadata.Track()
		trackName := strings.Trim(metadata.Title(), " ")

		if data[artistName] == nil {
			data[artistName] = &artist{
				name:         artistName,
				albums:       map[string]*album{},
				tracksByName: map[string]*track{},
			}
		}

		if data[artistName].albums[albumName] == nil {
			data[artistName].albums[albumName] = &album{
				name:         albumName,
				tracks:       map[int]map[int]*track{},
				tracksByName: map[string]*track{},
				artist:       data[artistName],
			}
		}

		if data[artistName].albums[albumName].tracks[diskNumber] == nil {
			data[artistName].albums[albumName].tracks[diskNumber] = map[int]*track{}
		}

		data[artistName].albums[albumName].tracks[diskNumber][trackNumber] = &track{
			name:        trackName,
			path:        path,
			trackNumber: trackNumber,
			diskNumber:  diskNumber,
			album:       data[artistName].albums[albumName],
		}

		data[artistName].albums[albumName].tracksByName[trackName] = data[artistName].albums[albumName].tracks[diskNumber][trackNumber]
		data[artistName].tracksByName[trackName] = data[artistName].albums[albumName].tracks[diskNumber][trackNumber]

		return nil
	})

	return &data
}

func getStats(data *artistMap) (artists uint, albums uint, tracks uint) {

	for _, artist := range *data {
		artists++
		for _, album := range artist.albums {
			albums++
			for _, disk := range album.tracks {
				for range disk {
					tracks++
				}
			}
		}
	}

	return
}

func overlapsAtTheEnd(t *track) bool {
	if len(t.silences) == 0 {
		return false
	}

	lastSilence := t.silences[len(t.silences)-1]
	difference := t.duration - lastSilence.end

	if difference < maxOverlapping && difference > tolerance {
		return true
	}

	return false
}

func overlapsAtTheBeginning(t *track) bool {
	if len(t.silences) == 0 {
		return false
	}

	firstSilence := t.silences[0]

	if firstSilence.start < maxOverlapping && firstSilence.start > tolerance {
		return true
	}

	return false
}

func truncatedAtTheBeginning(t *track) bool {
	if len(t.silences) == 0 {
		return true
	}

	firstSilence := t.silences[0]
	if firstSilence.start >= maxOverlapping {
		return true
	}

	return false
}

func truncatedAtTheEnd(t *track) bool {
	if len(t.silences) == 0 {
		return true
	}

	lastSilence := t.silences[len(t.silences)-1]
	difference := t.duration - lastSilence.end

	if difference >= maxOverlapping {
		return true
	}

	return false
}

func hugeSilenceAtTheBeginning(t *track) bool {
	if len(t.longSilences) == 0 {
		return false
	}

	firstSilence := t.longSilences[0]
	if firstSilence.start >= maxOverlapping {
		return false
	}

	return true
}

func hugeSilenceAtTheEnd(t *track) bool {
	if len(t.longSilences) == 0 {
		return false
	}

	lastSilence := t.longSilences[len(t.longSilences)-1]
	difference := t.duration - lastSilence.end

	if difference >= maxOverlapping {
		return false
	}

	return true
}

func plural(n uint, singular string, plural string) string {
	if n == 1 {
		return singular
	}

	return plural
}

func analyse(data *artistMap) {
	artists, albums, tracks := getStats(data)
	fmt.Printf("\nAnalysing %d %s, %d %s and %d %s...\n", artists, plural(artists, "artist", "artists"), albums, plural(albums, "album", "albums"), tracks, plural(tracks, "track", "tracks"))

	pb := progressbar.Default(int64(tracks))
	wg := sync.WaitGroup{}

	trackChannel := make(chan *track)

	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func() {
			for track := range trackChannel {
				track.silences, track.duration = getSilenceInfo(track.path, false)
				track.longSilences, _ = getSilenceInfo(track.path, true)
				pb.Add(1)
			}

			wg.Done()
		}()
	}

	for _, artist := range *data {
		for _, album := range artist.albums {
			for _, disk := range album.tracks {
				for _, track := range disk {
					trackChannel <- track
				}
			}
		}
	}

	close(trackChannel)
	wg.Wait()
	pb.Finish()
}

func countProblems(data *artistMap) (problems uint, fixable uint) {
	for _, artist := range *data {
		for _, album := range artist.albums {
			for _, disk := range album.tracks {
				for _, track := range disk {

					if truncatedAtTheBeginning(track) || truncatedAtTheEnd(track) {
						problems++
					} else if overlapsAtTheEnd(track) || overlapsAtTheBeginning(track) || hugeSilenceAtTheBeginning(track) || hugeSilenceAtTheEnd(track) {
						problems++
						fixable++
					}

				}
			}
		}
	}

	return
}

func logProblems(data *artistMap) {
	for _, artist := range *data {
		artistNameLogged := false

		for _, album := range artist.albums {
			albumNameLogged := false

			for _, disk := range album.tracks {
				for _, track := range disk {
					problems := ""

					if truncatedAtTheBeginning(track) {
						problems += color.RedString("T")
					} else {
						problems += "-"
					}

					if overlapsAtTheBeginning(track) {
						problems += color.YellowString("O")
					} else {
						problems += "-"
					}

					if hugeSilenceAtTheBeginning(track) {
						problems += color.HiMagentaString("S")
					} else {
						problems += "-"
					}

					problems += " "

					if hugeSilenceAtTheEnd(track) {
						problems += color.HiMagentaString("S")
					} else {
						problems += "-"
					}

					if overlapsAtTheEnd(track) {
						problems += color.YellowString("O")
					} else {
						problems += "-"
					}

					if truncatedAtTheEnd(track) {
						problems += color.RedString("T")
					} else {
						problems += "-"
					}

					if problems != "--- ---" {
						if !artistNameLogged {
							fmt.Print("\n  ", artist.name, "\n")
							artistNameLogged = true
						}

						if !albumNameLogged {
							fmt.Print("\n", album.name, "\n\n")
							albumNameLogged = true
						}

						fmt.Fprint(color.Output, problems, " ", track.name, "\n")
					}

				}
			}
		}
	}
}

func printProblemNumber(problems, fixable uint) {
	fmt.Println("")
	if problems == 0 {
		fmt.Println("No problems found")
	} else if problems == 1 {
		if fixable == 1 {
			fmt.Println("1 fixable problem found")
		} else {
			fmt.Println("1 non-fixable problem found")
		}
	} else if fixable == 0 {
		fmt.Printf("%d problems found, none of them are fixable\n", problems)
	} else if fixable == 1 {
		fmt.Printf("%d problems found, 1 of them is fixable\n", problems)
	} else {
		fmt.Printf("%d problems found, %d of them are fixable\n", problems, fixable)
	}
}

func fixProblems(fixable uint, data *artistMap) {
	fmt.Printf("Fixing %d %s...", fixable, plural(fixable, "problem", "problems"))

	pb := progressbar.Default(int64(fixable))
	wg := sync.WaitGroup{}

	trackChannel := make(chan *track)

	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func() {
			for track := range trackChannel {
				start := 0.0
				end := track.duration

				if overlapsAtTheBeginning(track) {
					firstSilence := track.silences[0]
					start = math.Max(start, firstSilence.start+silenceTolerance)
				}

				if hugeSilenceAtTheBeginning(track) {
					firstLongSilence := track.longSilences[0]
					start = math.Max(start, firstLongSilence.end-maxSilence)
				}

				if overlapsAtTheEnd(track) {
					lastSilence := track.silences[len(track.silences)-1]
					end = math.Min(end, lastSilence.end-silenceTolerance)
				}

				if hugeSilenceAtTheEnd(track) {
					lastLongSilence := track.longSilences[len(track.longSilences)-1]
					end = math.Min(end, lastLongSilence.start+maxSilence)
				}

				if start != 0 || end != track.duration {
					os.Remove(track.path + ".tmp.mp3")
					cmd := exec.Command("ffmpeg", "-ss", fmt.Sprintf("%f", start), "-t", fmt.Sprintf("%f", end-start), "-i", track.path, "-codec", "copy", track.path+".tmp.mp3")
					err := cmd.Run()
					if err == nil {
						os.Remove(track.path)
						os.Rename(track.path+".tmp.mp3", track.path)
					}
				}

				pb.Add(1)
			}

			wg.Done()
		}()
	}

	for _, artist := range *data {
		for _, album := range artist.albums {
			for _, disk := range album.tracks {
				for _, track := range disk {
					trackChannel <- track
				}
			}
		}
	}

	close(trackChannel)
	wg.Wait()
	pb.Finish()
	fmt.Println("")
}

var wrongChars = regexp.MustCompile("[\\/\\\\\\?\\%\\*\\:\\|\"<>]+")

func escape(path string) string {
	return wrongChars.ReplaceAllLiteralString(path, "-")
}

func sortData(folder string, data *artistMap) {
	for _, artist := range *data {
		os.Mkdir(filepath.Join(folder, escape(artist.name)), os.ModePerm)
		for _, album := range artist.albums {
			os.Mkdir(filepath.Join(folder, escape(artist.name), escape(album.name)), os.ModePerm)
			for diskNumber, disk := range album.tracks {
				for trackNumber, track := range disk {
					fileName := ""

					if len(album.tracks) > 1 {
						fileName += fmt.Sprintf("%02d - ", diskNumber)
					}

					fileName += fmt.Sprintf("%02d %s.mp3", trackNumber, escape(track.name))
					err := os.Rename(track.path, filepath.Join(folder, escape(artist.name), escape(album.name), fileName))
					if err != nil {
						fmt.Println(err)
					}
				}
			}
		}
	}
}

func main() {
	app := &cli.App{
		Name:  "mp3-check",
		Usage: "check your mp3 collection for faulty files",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "fix",
				Value: false,
				Usage: "fix found problems",
			},
			&cli.BoolFlag{
				Name:  "sort",
				Value: false,
				Usage: "sort music collection",
			},
		},
		Action: func(c *cli.Context) error {
			folder := "."
			if c.NArg() > 0 {
				folder = c.Args().Get(0)
			}

			data := getFolderData(folder)

			if c.Bool("fix") || !c.Bool("sort") {
				analyse(data)
			}

			if !c.Bool("fix") && !c.Bool("sort") {
				logProblems(data)
			}

			var problems, fixable uint

			if c.Bool("fix") || !c.Bool("sort") {
				problems, fixable = countProblems(data)
				printProblemNumber(problems, fixable)
			}

			if c.Bool("fix") {
				fixProblems(fixable, data)
			}

			if c.Bool("sort") {
				sortData(folder, data)
			}

			return nil
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
