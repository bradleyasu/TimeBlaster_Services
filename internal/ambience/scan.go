// Package ambience is the television's ambience mode: looping local videos --
// a fireplace, an aquarium, rain -- chosen with the channel knob.
//
// Each channel is a folder:
//
//	ambient/ch1/fireplace.mp4
//	ambient/ch2/1.mp4
//	ambient/ch2/2.mp4
//
// A folder's videos play in natural order and repeat. A folder holding a single
// video loops it seamlessly.
package ambience

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Channel is one ambience folder.
type Channel struct {
	// Number orders the channels on the knob. It comes from the folder name,
	// so ch2 is the second position even if ch1 is missing.
	Number int `json:"number"`
	// Name is the optional part of the folder name after the number:
	// "ch1-fireplace" is named "Fireplace".
	Name string `json:"name,omitempty"`
	// Dir is the folder's path.
	Dir string `json:"-"`
}

// Label renders the channel for the screen: "AMBIENCE 1 FIREPLACE".
func (c Channel) Label() string {
	if c.Name == "" {
		return fmt.Sprintf("AMBIENCE %d", c.Number)
	}
	return fmt.Sprintf("AMBIENCE %d %s", c.Number, strings.ToUpper(c.Name))
}

// folderPattern matches ch1, CH01, ch1-fireplace, ch1 fireplace, ch1_fire_place.
var folderPattern = regexp.MustCompile(`(?i)^ch0*(\d{1,4})(?:[ _-]+(.*))?$`)

// videoExtensions are the files a channel plays. Anything else in the folder --
// a README, a thumbnail -- is ignored rather than handed to mpv to fail on.
var videoExtensions = map[string]bool{
	".mp4": true, ".m4v": true, ".mov": true, ".mkv": true, ".webm": true,
}

// Scan reads the ambience channels under root. A missing root is no channels,
// not an error: it is how a Timeblaster with no ambience videos looks. Folders
// with no videos in them are skipped, so the knob never has a dead position.
//
// skipped describes anything ignored for a reason a person would want to know
// about, such as two folders claiming the same number.
func Scan(root string) (channels []Channel, skipped []string, err error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("ambience: reading %s: %w", root, err)
	}

	byNumber := map[int]Channel{}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		// Stat rather than e.IsDir, so a symlink to a folder elsewhere -- on a
		// USB stick, say -- counts.
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		m := folderPattern.FindStringSubmatch(e.Name())
		if m == nil {
			if !strings.HasPrefix(e.Name(), ".") {
				skipped = append(skipped, fmt.Sprintf("%s: not named ch<number>", e.Name()))
			}
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n == 0 {
			skipped = append(skipped, fmt.Sprintf("%s: channels are numbered from 1", e.Name()))
			continue
		}
		if videos, err := Videos(dir); err != nil || len(videos) == 0 {
			skipped = append(skipped, fmt.Sprintf("%s: no videos", e.Name()))
			continue
		}
		ch := Channel{Number: n, Name: displayName(m[2]), Dir: dir}
		if prev, dup := byNumber[n]; dup {
			// Keep the first in name order, so the choice is stable rather than
			// whatever order the directory happened to list.
			keep, drop := prev, ch
			if filepath.Base(ch.Dir) < filepath.Base(prev.Dir) {
				keep, drop = ch, prev
			}
			byNumber[n] = keep
			skipped = append(skipped, fmt.Sprintf("%s: channel %d is already %s",
				filepath.Base(drop.Dir), n, filepath.Base(keep.Dir)))
			continue
		}
		byNumber[n] = ch
	}

	for _, ch := range byNumber {
		channels = append(channels, ch)
	}
	sort.Slice(channels, func(i, j int) bool { return channels[i].Number < channels[j].Number })
	sort.Strings(skipped)
	return channels, skipped, nil
}

// Videos lists a channel folder's videos in the order they play.
//
// Hidden files are skipped: copying from a Mac leaves "._1.mp4" beside every
// video, which looks like a video by its name and is not one.
func Videos(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !videoExtensions[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		out = append(out, path)
	}
	sort.Slice(out, func(i, j int) bool {
		return naturalLess(filepath.Base(out[i]), filepath.Base(out[j]))
	})
	return out, nil
}

// displayName turns "fire_place" into "Fire Place".
func displayName(s string) string {
	s = strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(s))
	words := strings.Fields(s)
	for i, w := range words {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

// naturalLess orders names the way a person numbering files expects: runs of
// digits compare by value, so 2.mp4 comes before 10.mp4. Plain string order
// would play 1, 10, 11, 2 -- correct alphabetically and wrong in every way
// that matters. Ties fall back to plain order, so the result is total.
func naturalLess(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(la) && j < len(lb) {
		ca, cb := la[i], lb[j]
		if isDigit(ca) && isDigit(cb) {
			si := i
			for i < len(la) && isDigit(la[i]) {
				i++
			}
			sj := j
			for j < len(lb) && isDigit(lb[j]) {
				j++
			}
			na := strings.TrimLeft(la[si:i], "0")
			nb := strings.TrimLeft(lb[sj:j], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if ca != cb {
			return ca < cb
		}
		i++
		j++
	}
	if len(la)-i != len(lb)-j {
		return len(la)-i < len(lb)-j
	}
	return a < b
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
