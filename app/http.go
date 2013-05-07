package gae

import (
	"appengine"
	"appengine/blobstore"
	"appengine/datastore"
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Walk struct {
	Name        string
	Date        time.Time
	SNMPBlobKey appengine.BlobKey
	SAPBlobKey  appengine.BlobKey
}

var templates = template.Must(template.ParseFiles("tmpl/root.html"))

func init() {
	http.HandleFunc("/", root)
	http.HandleFunc("/uploadurl", uploadurl)
	http.HandleFunc("/upload", upload)
	http.HandleFunc("/download", download)
}

func root(w http.ResponseWriter, r *http.Request) {
	err := templates.ExecuteTemplate(w, "root.html", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func uploadurl(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	uploadURL, err := blobstore.UploadURL(c, "/upload", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	fmt.Fprint(w, uploadURL)
}

func sapline(o string, t string, d string) string {
	o = strings.TrimLeft(o, ".")

	d = strings.TrimSpace(d)
	d = strings.Trim(d, "\"'")

	if t == "STRING" {
		t = "OctetString"

		if strings.Contains(d, "\n") {
			d = fmt.Sprintf("0x%s", hex.EncodeToString([]byte(d)))
		}
	} else if t == "Timeticks" {
		t = "TimeTicks"
		d = strings.TrimLeft(strings.Split(d, ")")[0], "(")
	} else if t == "OID" {
		t = "ObjectID"
		d = strings.TrimLeft(d, ".")
	} else if t == "INTEGER" {
		t = "Integer"
	} else if t == "Hex-STRING" {
		t = "OctetString"
		d = fmt.Sprintf("0x%s", strings.Replace(d, " ", "", -1))
	}

	if strings.HasSuffix(t, "32") {
		t = t[0 : len(t)-2]
	}

	return strings.Join([]string{o, t, d}, ", ")
}

func ConvertToSAP(r io.Reader, w io.Writer) {
	typed_match := regexp.MustCompile("^(\\.[^ ]+) = ([^:]+): (.*)")
	untyped_match := regexp.MustCompile("^(\\.[^ ]+) = (.*)")

	var current_oid string
	var current_type string
	var current_data string

	br := bufio.NewReader(r)
	bw := bufio.NewWriter(w)

	for {
		line, err := br.ReadString('\n')
		if err == io.EOF {
			if len(current_data) != 0 {
				fmt.Fprintf(bw, "%s\n", sapline(current_oid, current_type, current_data))
			}

			break
		} else if err != nil {
			break
		}

		if strings.Contains(line, "No more variables left in this MIB View") {
			continue
		}

		groups := typed_match.FindStringSubmatch(line)
		if groups != nil {
			if len(current_oid) != 0 {
				fmt.Fprintf(bw, "%s\n", sapline(current_oid, current_type, current_data))
			}

			current_oid = groups[1]
			current_type = groups[2]
			current_data = groups[3]
		} else {
			groups := untyped_match.FindStringSubmatch(line)
			if groups != nil {
				if len(current_oid) != 0 {
					fmt.Fprintf(bw, "%s\n", sapline(current_oid, current_type, current_data))
				}

				current_oid = groups[1]
				current_data = groups[2]

				if _, err := strconv.ParseInt(current_data, 0, 64); err != nil {
					current_type = "STRING"
				} else {
					current_type = "INTEGER"
				}
			} else {
				current_data = current_data + line
			}
		}
	}

	fmt.Fprintf(bw, sapline(current_oid, current_type, current_data))

	bw.Flush()
}

func upload(w http.ResponseWriter, r *http.Request) {
	blob_map, _, err := blobstore.ParseUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	c := appengine.NewContext(r)

	keys := make([]string, 0)

	for _, blobs := range blob_map {
		for _, blob := range blobs {
			rd := blobstore.NewReader(c, blob.BlobKey)

			wr, err := blobstore.Create(c, blob.ContentType)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}

			ConvertToSAP(rd, wr)

			wr.Close()

			blobkey, err := wr.Key()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}

			walk := Walk{
				Name:        strings.Split(blob.Filename, ".")[0],
				Date:        time.Now(),
				SNMPBlobKey: blob.BlobKey,
				SAPBlobKey:  blobkey,
			}

			key, err := datastore.Put(c, datastore.NewIncompleteKey(c, "walk", nil), &walk)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			keys = append(keys, key.Encode())
		}
	}

	json, err := json.Marshal(keys)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	fmt.Fprintf(w, "%s", json)
}

func download(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	key, err := datastore.DecodeKey(r.FormValue("key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	var walk Walk
	if err := datastore.Get(c, key, &walk); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	blobinfo, err := blobstore.Stat(c, walk.SAPBlobKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	header := w.Header()
	header.Set("content-type", blobinfo.ContentType)
	header.Set("content-disposition", fmt.Sprintf("attachment; filename=%s.sapwalk2", walk.Name))
	blobstore.Send(w, walk.SAPBlobKey)
}
