package main

import (
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/fatih/color"
	"github.com/shkh/lastfm-go/lastfm"

	"github.com/dhowden/tag"
	"github.com/tcolgate/mp3"
	"github.com/urfave/cli/v2"
)

var wrongChars = regexp.MustCompile("[\\/\\\\\\?\\%\\*\\:\\|\"<>]+")

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
	name        string
	path        string
	trackNumber int
	diskNumber  int
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

func escape(path string) string {
	return wrongChars.ReplaceAllLiteralString(path, "-")
}

func getDuration(path string) float64 {
	t := 0.0

	file, err := os.Open(path)
	if err != nil {
		return t
	}

	defer file.Close()

	d := mp3.NewDecoder(file)
	var f mp3.Frame
	skipped := 0

	for {

		if err := d.Decode(&f, &skipped); err != nil {
			if err == io.EOF {
				break
			}

			return 0
		}

		t = t + f.Duration().Seconds()
	}

	return t
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

		artistName := strings.Trim(strings.Split(metadata.Artist(), ",")[0], " ")
		albumName := strings.Trim(metadata.Album(), " ")
		diskNumber, _ := metadata.Disc()
		trackNumber, _ := metadata.Track()
		trackName := strings.Trim(metadata.Title(), " ")

		normalizedArtistName := normalize(artistName)
		normalizedAlbumName := normalize(albumName)
		normalizedTrackName := normalize(trackName)

		if data[normalizedArtistName] == nil {
			data[normalizedArtistName] = &artist{
				name:         artistName,
				albums:       map[string]*album{},
				tracksByName: map[string]*track{},
			}
		}

		if data[normalizedArtistName].albums[normalizedAlbumName] == nil {
			data[normalizedArtistName].albums[normalizedAlbumName] = &album{
				name:         albumName,
				tracks:       map[int]map[int]*track{},
				tracksByName: map[string]*track{},
				artist:       data[normalizedArtistName],
			}
		}

		if data[normalizedArtistName].albums[normalizedAlbumName].tracks[diskNumber] == nil {
			data[normalizedArtistName].albums[normalizedAlbumName].tracks[diskNumber] = map[int]*track{}
		}

		data[normalizedArtistName].albums[normalizedAlbumName].tracks[diskNumber][trackNumber] = &track{
			name:        trackName,
			path:        path,
			trackNumber: trackNumber,
			diskNumber:  diskNumber,
			album:       data[normalizedArtistName].albums[normalizedAlbumName],
		}

		data[normalizedArtistName].albums[normalizedAlbumName].tracksByName[normalizedTrackName] = data[normalizedArtistName].albums[normalizedAlbumName].tracks[diskNumber][trackNumber]
		data[normalizedArtistName].tracksByName[normalizedTrackName] = data[normalizedArtistName].albums[normalizedAlbumName].tracks[diskNumber][trackNumber]

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

func normalize(name string) string {
	return strings.Trim(strings.ToLower(name), " ")
}

func logProblems(data *artistMap, api *lastfm.Api) {
	for _, artist := range *data {
		artistNameLogged := false

		for _, album := range artist.albums {
			wrongTracks := []*wrongTrack{}
			missingTracks := []*missingTrack{}
			unmatched := map[*track]bool{}

			for _, disk := range album.tracks {
				for _, track := range disk {
					unmatched[track] = true
				}
			}

			albumInfo, err := api.Album.GetInfo(lastfm.P{
				"artist":      artist.name,
				"album":       album.name,
				"autocorrect": "1",
			})

			logAlbumProblems := func() {
				if len(missingTracks) > 0 || len(wrongTracks) > 0 || len(unmatched) > 0 {
					if !artistNameLogged {
						artistNameLogged = true
						fmt.Print("\n  ", artist.name, "\n\n")
					}

					fmt.Print(album.name, "\n\n")

					for _, wrong := range wrongTracks {
						fmt.Fprint(color.Output, color.RedString(fmt.Sprintf("%+4d", int64(math.Round(wrong.duration-wrong.expectedDuration)))), " ", wrong.name, "\n")
					}

					for _, missing := range missingTracks {
						fmt.Fprint(color.Output, color.YellowString("miss"), " ", missing.name, "\n")
					}

					for um := range unmatched {
						fmt.Fprint(color.Output, color.CyanString(" 404"), " ", um.name, "\n")
					}

					fmt.Print("\n")
				}
			}

			if err != nil {
				logAlbumProblems()
				continue
			}

			for _, track := range albumInfo.Tracks {
				expectedDuration, _ := strconv.ParseFloat(track.Duration, 64)
				dataTrack := album.tracksByName[normalize(track.Name)]

				if dataTrack == nil {
					missingTracks = append(missingTracks, &missingTrack{
						name:  track.Name,
						album: album,
					})
				} else {
					delete(unmatched, dataTrack)
					duration := getDuration(dataTrack.path)
					if duration < expectedDuration-0.5 || duration > expectedDuration+5 {
						wrongTracks = append(wrongTracks, &wrongTrack{
							expectedDuration: expectedDuration,
							track:            dataTrack,
							duration:         duration,
						})
					}
				}
			}

			logAlbumProblems()

		}

	}

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
		Usage: "check your mp3 collection against last.fm",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "key",
				Aliases: []string{"k"},
				EnvVars: []string{"LASTFM_API_KEY"},
				Value:   "",
				Usage:   "last.fm API key",
			},
			&cli.StringFlag{
				Name:    "secret",
				Aliases: []string{"s"},
				EnvVars: []string{"LASTFM_API_SECRET"},
				Value:   "",
				Usage:   "last.fm API secret",
			},
		},
		Commands: []*cli.Command{
			{
				Name:    "problems",
				Aliases: []string{"p"},
				Usage:   "check for tracks with the wrong length or albums with missing songs",
				Action: func(c *cli.Context) error {
					folder := "."
					if c.NArg() > 0 {
						folder = c.Args().Get(0)
					}

					api := lastfm.New(c.String("key"), c.String("secret"))

					data := getFolderData(folder)
					logProblems(data, api)
					return nil
				},
			},
			{
				Name:    "sort",
				Aliases: []string{"s"},
				Usage:   "sorts your music collection",
				Action: func(c *cli.Context) error {

					folder := "."
					if c.NArg() > 0 {
						folder = c.Args().Get(0)
					}

					data := getFolderData(folder)
					sortData(folder, data)
					return nil
				},
			},
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
