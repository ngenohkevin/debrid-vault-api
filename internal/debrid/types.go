package debrid

// User represents account info from a debrid provider.
type User struct {
	ID         int    `json:"id"`
	Username   string `json:"username"`
	Email      string `json:"email"`
	Premium    int    `json:"premium"`
	Expiration string `json:"expiration"`
	Type       string `json:"type"`
}

// TorrentFile represents a file within a torrent.
type TorrentFile struct {
	ID       int    `json:"id"`
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	Selected int    `json:"selected"`
}

// Torrent represents a torrent entry in the debrid cloud.
type Torrent struct {
	ID       string        `json:"id"`
	Filename string        `json:"filename"`
	Hash     string        `json:"hash"`
	Bytes    int64         `json:"bytes"`
	Host     string        `json:"host"`
	Split    int           `json:"split"`
	Progress float64       `json:"progress"`
	Status   string        `json:"status"`
	Added    string        `json:"added"`
	Links    []string      `json:"links"`
	Ended    string        `json:"ended"`
	Speed    int64         `json:"speed"`
	Seeders  int           `json:"seeders"`
	Files    []TorrentFile `json:"files,omitempty"`
}

// Download represents a completed download/unrestricted link.
type Download struct {
	ID         string `json:"id"`
	Filename   string `json:"filename"`
	MimeType   string `json:"mimeType"`
	Filesize   int64  `json:"filesize"`
	Link       string `json:"link"`
	Host       string `json:"host"`
	Download   string `json:"download"`
	Generated  string `json:"generated"`
	Streamable int    `json:"streamable"`
}

// UnrestrictedLink represents a direct download link.
type UnrestrictedLink struct {
	ID         string `json:"id"`
	Filename   string `json:"filename"`
	MimeType   string `json:"mimeType"`
	Filesize   int64  `json:"filesize"`
	Link       string `json:"link"`
	Host       string `json:"host"`
	Download   string `json:"download"`
	Streamable int    `json:"streamable"`
}

// AddMagnetResponse is returned when adding a magnet link.
type AddMagnetResponse struct {
	ID  string `json:"id"`
	URI string `json:"uri"`
}
