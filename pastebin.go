// Package pastebin is a simple modern and powerful pastebin service
package main

import (
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"

	duration "github.com/ChannelMeter/iso8601duration"
	// uniuri is used for easy random string generation
	"github.com/dchest/uniuri"
	// pygments is used for syntax highlighting
	"github.com/ewhal/pygments"
	// mysql driver
	_ "github.com/go-sql-driver/mysql"
	// mux is used for url routing
	"github.com/gorilla/mux"
)

type Configuration struct {
	// ADDRESS that pastebin will return links for
	Address string
	// LENGTH of paste id
	Length int
	// PORT that pastebin will listen on
	Port string
	// USERNAME for database
	Username string
	// PASS database password
	Password string
	// NAME database name
	Name string
}

var configuration Configuration

// DATABASE connection String
var DATABASE string

// Template pages
var templates = template.Must(template.ParseFiles("assets/paste.html", "assets/index.html", "assets/clone.html"))
var syntax, _ = ioutil.ReadFile("assets/syntax.html")

// Response API struct
type Response struct {
	SUCCESS bool   `json:"success"`
	STATUS  string `json:"status"`
	ID      string `json:"id"`
	TITLE   string `json:"title"`
	SHA1    string `json:"sha1"`
	URL     string `json:"url"`
	SIZE    int    `json:"size"`
	DELKEY  string `json:"delkey"`
}

// Page generation struct
type Page struct {
	Title    string
	Body     []byte
	Raw      string
	Home     string
	Download string
	Clone    string
}

// check error handling function
func Check(err error) {
	if err != nil {
		log.Println(err)
	}
}

// GenerateName uses uniuri to generate a random string that isn't in the
// database
func GenerateName() string {
	// use uniuri to generate random string
	// hardcode this for now until I figure out why json isn't parsing correctly
	id := uniuri.NewLen(6)

	db, err := sql.Open("mysql", DATABASE)
	Check(err)
	defer db.Close()
	// query database if id exists and if it does call generateName again
	query, err := db.Query("select id from pastebin where id=?", id)
	if err != sql.ErrNoRows {
		for query.Next() {
			GenerateName()
		}
	}

	return id

}

// Sha1 hashes paste into a sha1 hash
func Sha1(paste string) string {
	hasher := sha1.New()

	hasher.Write([]byte(paste))
	sha := base64.URLEncoding.EncodeToString(hasher.Sum(nil))
	return sha
}

// DurationFromExpiry takes the expiry in string format and returns the duration
// that the paste will exist for
func DurationFromExpiry(expiry string) time.Duration {
	if expiry == "" {
		expiry = "P20Y"
	}
	dura, err := duration.FromString(expiry) // dura is time.Duration type
	Check(err)

	duration := dura.ToDuration()

	return duration
}

// Save function handles the saving of each paste.
// raw string is the raw paste input
// lang string is the user specified language for syntax highlighting
// title string user customized title
// expiry string duration that the paste will exist for
// Returns Response struct
func Save(raw string, lang string, title string, expiry string) Response {

	db, err := sql.Open("mysql", DATABASE)
	Check(err)
	defer db.Close()

	// hash paste data and query database to see if paste exists
	sha := Sha1(raw)
	query, err := db.Query("select id, title, hash, data, delkey from pastebin where hash=?", sha)

	if err != sql.ErrNoRows {
		for query.Next() {
			var id, title, hash, paste, delkey string
			err := query.Scan(&id, &title, &hash, &paste, &delkey)
			Check(err)
			url := configuration.Address + "/p/" + id
			return Response{true, "saved", id, title, hash, url, len(paste), delkey}
		}
	}
	id := GenerateName()
	url := configuration.Address + "/p/" + id
	if lang != "" {
		url += "/" + lang
	}

	const timeFormat = "2006-01-02 15:04:05"
	expiryTime := time.Now().Add(DurationFromExpiry(expiry)).Format(timeFormat)

	delKey := uniuri.NewLen(40)
	dataEscaped := html.EscapeString(raw)

	stmt, err := db.Prepare("INSERT INTO pastebin(id, title, hash, data, delkey, expiry) values(?,?,?,?,?,?)")
	Check(err)
	if title == "" {
		title = id
	}
	_, err = stmt.Exec(id, html.EscapeString(title), sha, dataEscaped, delKey, expiryTime)
	Check(err)

	return Response{true, "saved", id, title, sha, url, len(dataEscaped), delKey}
}

// DelHandler checks to see if delkey and pasteid exist in the database.
// if both exist and are correct the paste will be removed.
func DelHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["pasteId"]
	delkey := r.FormValue("delkey")

	db, err := sql.Open("mysql", DATABASE)
	Check(err)
	defer db.Close()

	stmt, err := db.Prepare("delete from pastebin where delkey=? and id=?")
	Check(err)

	res, err := stmt.Exec(html.EscapeString(delkey), html.EscapeString(id))
	Check(err)

	_, err = res.RowsAffected()
	if err != sql.ErrNoRows {
		w.Header().Set("Content-Type", "application/json")
		b := Response{STATUS: "DELETED " + id}
		err := json.NewEncoder(w).Encode(b)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
}

// SaveHandler Handles saving pastes and outputing responses
func SaveHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	output := vars["output"]
	switch r.Method {
	case "POST":
		paste := r.FormValue("p")
		lang := r.FormValue("lang")
		title := r.FormValue("title")
		expiry := r.FormValue("expiry")
		if paste == "" {
			http.Error(w, "Empty paste", 500)
			return
		}
		b := Save(paste, lang, title, expiry)

		switch output {
		case "redirect":
			http.Redirect(w, r, b.URL, 301)

		default:
			w.Header().Set("Content-Type", "application/json")
			err := json.NewEncoder(w).Encode(b)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

		}
	}

}

// Highlight uses user specified input to call pygments library to highlight the
// paste
func Highlight(s string, lang string) (string, error) {

	highlight, err := pygments.Highlight(html.UnescapeString(s), html.EscapeString(lang), "html", "style=autumn,linenos=True, lineanchors=True,anchorlinenos=True,noclasses=True,", "utf-8")
	if err != nil {
		return "", err
	}
	return highlight, nil

}

// GetPaste takes pasteid and language
// queries the database and returns paste data
func GetPaste(paste string, lang string) (string, string) {
	param1 := html.EscapeString(paste)
	db, err := sql.Open("mysql", DATABASE)
	Check(err)
	defer db.Close()
	var title, s string
	var expiry string
	err = db.QueryRow("select title, data, expiry from pastebin where id=?", param1).Scan(&title, &s, &expiry)
	Check(err)
	if time.Now().Format("2006-01-02 15:04:05") >= expiry {
		stmt, err := db.Prepare("delete from pastebin where id=?")
		Check(err)
		_, err = stmt.Exec(param1)
		Check(err)
		return "Error invalid paste", ""
	}

	if err == sql.ErrNoRows {
		return "Error invalid paste", ""
	}
	if lang != "" {
		high, err := Highlight(s, lang)
		Check(err)
		return high, html.UnescapeString(title)
	}
	return html.UnescapeString(s), html.UnescapeString(title)
}

// APIHandler handles get requests of pastes
func APIHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	paste := vars["pasteId"]

	b, _ := GetPaste(paste, "")
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(b)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

}

// PasteHandler handles the generation of paste pages with the links
func PasteHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	paste := vars["pasteId"]
	lang := vars["lang"]

	s, title := GetPaste(paste, lang)

	// button links
	link := configuration.Address + "/raw/" + paste
	download := configuration.Address + "/download/" + paste
	clone := configuration.Address + "/clone/" + paste
	// Page struct
	p := &Page{
		Title:    title,
		Body:     []byte(s),
		Raw:      link,
		Home:     configuration.Address,
		Download: download,
		Clone:    clone,
	}
	if lang == "" {

		err := templates.ExecuteTemplate(w, "paste.html", p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

	} else {
		fmt.Fprintf(w, string(syntax), p.Title, p.Title, s, p.Home, p.Download, p.Raw, p.Clone)

	}
}

// CloneHandler handles generating the clone pages
func CloneHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	paste := vars["pasteId"]

	s, title := GetPaste(paste, "")

	// Page links
	link := configuration.Address + "/raw/" + paste
	download := configuration.Address + "/download/" + paste
	clone := configuration.Address + "/clone/" + paste

	// Clone page struct
	p := &Page{
		Title:    title,
		Body:     []byte(s),
		Raw:      link,
		Home:     configuration.Address,
		Download: download,
		Clone:    clone,
	}
	err := templates.ExecuteTemplate(w, "clone.html", p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

}

// DownloadHandler forces downloads of selected pastes
func DownloadHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	paste := vars["pasteId"]
	s, _ := GetPaste(paste, "")

	// Set header to an attachment so browser will automatically download it
	w.Header().Set("Content-Disposition", "attachment; filename="+paste)
	w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
	io.WriteString(w, s)

}

// RawHandler displays the pastes in text/plain format
func RawHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	paste := vars["pasteId"]
	s, _ := GetPaste(paste, "")

	w.Header().Set("Content-Type", "text/plain; charset=UTF-8; imeanit=yes")
	// simply write string to browser
	io.WriteString(w, s)

}

// RootHandler handles generating the root page
func RootHandler(w http.ResponseWriter, r *http.Request) {
	err := templates.ExecuteTemplate(w, "index.html", &Page{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func main() {
	file, err := os.Open("config.json")
	if err != nil {
		panic(err)
	}
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&configuration)
	if err != nil {
		panic(err)
	}

	DATABASE = configuration.Username + ":" + configuration.Password + "@/" + configuration.Name + "?charset=utf8"
	// create new mux router
	router := mux.NewRouter()

	// serverside rending stuff
	router.HandleFunc("/p/{pasteId}", PasteHandler).Methods("GET")
	router.HandleFunc("/raw/{pasteId}", RawHandler).Methods("GET")
	router.HandleFunc("/p/{pasteId}/{lang}", PasteHandler).Methods("GET")
	router.HandleFunc("/clone/{pasteId}", CloneHandler).Methods("GET")
	router.HandleFunc("/download/{pasteId}", DownloadHandler).Methods("GET")
	// api
	router.HandleFunc("/api", SaveHandler).Methods("POST")
	router.HandleFunc("/api/{output}", SaveHandler).Methods("POST")
	router.HandleFunc("/api/{pasteid}", APIHandler).Methods("GET")
	router.HandleFunc("/api/{pasteId}", DelHandler).Methods("DELETE")
	router.HandleFunc("/", RootHandler)
	err = http.ListenAndServe(configuration.Port, router)
	if err != nil {
		log.Fatal(err)
	}

}
