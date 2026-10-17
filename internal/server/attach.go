package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"homeport/internal/assist"
)

// MARK: Files and pictures in the chat
//
// Pictures, text files and PDFs dropped or pasted into a chat box come along with the message: pictures as pictures
// (for a model that sees them), the others as their text. The page shrinks big photos before sending them.

// fileIn is one file from the page: data is base64.
type fileIn struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Data string `json:"data"`
}

const (
	maxPictures    = 6
	maxPictureSize = 8 << 20
	maxFileText    = 120_000 // characters of one file's text
	maxChatBody    = 40 << 20
)

var pictureTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// decodeChat is decode with room for files.
func decodeChat(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxChatBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return errors.New("the files are too big to send together (40 MB at most)")
		}
		return fmt.Errorf("bad request: %w", err)
	}
	return nil
}

// attachments reads the files sent with a message.
func attachments(ctx context.Context, files []fileIn) (assist.Attachments, error) {
	var att assist.Attachments
	var texts []string
	for _, f := range files {
		name := strings.Map(func(r rune) rune {
			if r < 0x20 || r == '`' {
				return -1
			}
			return r
		}, f.Name)
		name = cmpOr(name, "file")
		data, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			return att, fmt.Errorf("%s didn't arrive whole: send it again", name)
		}
		typ := strings.ToLower(strings.TrimSpace(strings.Split(f.Type, ";")[0]))
		if sniffed := http.DetectContentType(data); pictureTypes[sniffed] {
			typ = sniffed
		}
		switch {
		case pictureTypes[typ]:
			if len(att.Images) >= maxPictures {
				return att, fmt.Errorf("send at most %d pictures at a time", maxPictures)
			}
			if len(data) > maxPictureSize {
				return att, fmt.Errorf("%s is too big (8 MB at most)", name)
			}
			att.Images = append(att.Images, assist.Image{Name: name, Type: typ, Data: data})
		case strings.HasPrefix(typ, "image/"):
			return att, fmt.Errorf("%s: pictures can be JPEG, PNG, GIF or WebP", name)
		case typ == "application/pdf" || bytes.HasPrefix(data, []byte("%PDF-")):
			text, err := pdfText(ctx, data)
			if err != nil {
				return att, fmt.Errorf("%s: %w", name, err)
			}
			texts = append(texts, fileBlock(name, text))
			att.Files = append(att.Files, name)
		case utf8.Valid(data) && !bytes.ContainsRune(data, 0):
			texts = append(texts, fileBlock(name, string(data)))
			att.Files = append(att.Files, name)
		default:
			return att, fmt.Errorf("%s isn't something the AI can read: send pictures, PDFs or text files (notes, logs, code, CSV)", name)
		}
	}
	att.Text = strings.Join(texts, "\n\n")
	return att, nil
}

func fileBlock(name, text string) string {
	text = strings.TrimSpace(text)
	if r := []rune(text); len(r) > maxFileText {
		text = string(r[:maxFileText]) + "\n… (the rest of the file was left out: it's long)"
	}
	return "File " + name + ":\n```\n" + text + "\n```"
}

// pdfText is a PDF's text (pdftotext, from poppler-utils).
func pdfText(ctx context.Context, data []byte) (string, error) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		return "", errors.New("reading PDFs needs poppler-utils on the server (sudo apt install poppler-utils)")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pdftotext", "-layout", "-enc", "UTF-8", "-", "-")
	cmd.Stdin = bytes.NewReader(data)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("couldn't read it as a PDF (%s)", cmpOr(strings.TrimSpace(errOut.String()), err.Error()))
	}
	if strings.TrimSpace(out.String()) == "" {
		return "", errors.New("it has no text to read (a scan?): send its pages as pictures instead")
	}
	return out.String(), nil
}

// chatImage sends picture n of a turn of the caller's conversation.
func (s *Server) chatImage(w http.ResponseWriter, r *http.Request) {
	turn, _ := strconv.Atoi(r.PathValue("turn"))
	n, _ := strconv.Atoi(r.PathValue("n"))
	im, err := s.Chat.Image(actor(r).Name, r.PathValue("id"), turn, n)
	if err != nil {
		chatError(w, err)
		return
	}
	w.Header().Set("Content-Type", im.Type)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(im.Data)
}
