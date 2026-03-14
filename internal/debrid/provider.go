package debrid

// Provider defines the interface for debrid services (Real-Debrid, TorBox, etc).
type Provider interface {
	// GetUser returns account information.
	GetUser() (*User, error)

	// ListTorrents returns all torrents in the cloud.
	ListTorrents() ([]Torrent, error)

	// GetTorrentInfo returns details for a specific torrent.
	GetTorrentInfo(id string) (*Torrent, error)

	// AddMagnet adds a magnet link to the cloud.
	AddMagnet(magnet string) (*AddMagnetResponse, error)

	// SelectFiles selects which files to download from a torrent.
	SelectFiles(torrentID string, files string) error

	// DeleteTorrent removes a torrent from the cloud.
	DeleteTorrent(id string) error

	// UnrestrictLink generates a direct download URL for a link.
	UnrestrictLink(link string) (*UnrestrictedLink, error)

	// ListDownloads returns download history.
	ListDownloads(limit int) ([]Download, error)

	// InvalidateCache clears any cached data.
	InvalidateCache()

	// Name returns the provider name (e.g., "Real-Debrid", "TorBox").
	Name() string
}
