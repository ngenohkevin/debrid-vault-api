package realdebrid

import "github.com/ngenohkevin/debrid-vault-api/internal/debrid"

// Re-export debrid types for backward compatibility.
type User = debrid.User
type TorrentFile = debrid.TorrentFile
type Torrent = debrid.Torrent
type Download = debrid.Download
type UnrestrictedLink = debrid.UnrestrictedLink
type AddMagnetResponse = debrid.AddMagnetResponse

type APIError struct {
	ErrorCode int    `json:"error_code"`
	Error     string `json:"error"`
}
