package realdebrid

type User struct {
	ID         int    `json:"id"`
	Username   string `json:"username"`
	Email      string `json:"email"`
	Premium    int    `json:"premium"`
	Expiration string `json:"expiration"`
	Type       string `json:"type"`
}

type Torrent struct {
	ID       string   `json:"id"`
	Filename string   `json:"filename"`
	Hash     string   `json:"hash"`
	Bytes    int64    `json:"bytes"`
	Host     string   `json:"host"`
	Split    int      `json:"split"`
	Progress float64  `json:"progress"`
	Status   string   `json:"status"`
	Added    string   `json:"added"`
	Links    []string `json:"links"`
	Ended    string   `json:"ended"`
	Speed    int64    `json:"speed"`
	Seeders  int      `json:"seeders"`
}

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

type AddMagnetResponse struct {
	ID  string `json:"id"`
	URI string `json:"uri"`
}

type APIError struct {
	ErrorCode int    `json:"error_code"`
	Error     string `json:"error"`
}
