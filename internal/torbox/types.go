package torbox

// tbResponse wraps all TorBox API responses.
type tbResponse[T any] struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Data    T      `json:"data"`
}

type tbUser struct {
	ID        int    `json:"id"`
	Email     string `json:"email"`
	Plan      int    `json:"plan"`
	PlanName  string `json:"plan_name"`
	Expiry    string `json:"premium_expires_at"`
	TotalUsed int64  `json:"total_bytes_downloaded"`
}

type tbTorrent struct {
	ID               int      `json:"id"`
	Hash             string   `json:"hash"`
	Name             string   `json:"name"`
	Size             int64    `json:"size"`
	Progress         float64  `json:"progress"`
	DownloadState    string   `json:"download_state"`
	DownloadSpeed    int64    `json:"download_speed"`
	Seeds            int      `json:"seeds"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
	Files            []tbFile `json:"files"`
	DownloadFinished bool     `json:"download_finished"`
	InactiveCheck    int      `json:"inactive_check"`
}

type tbFile struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	ShortName string `json:"short_name"`
	MimeType  string `json:"mimetype"`
}

type tbCreateTorrent struct {
	TorrentID int    `json:"torrent_id"`
	Name      string `json:"name"`
	Hash      string `json:"hash"`
}

type tbDownloadLink struct {
	Link string `json:"data"`
}
