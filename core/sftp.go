package core

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SFTPClient wraps an sftp.Client for file operations
type SFTPClient struct {
	client *sftp.Client
}

// FileEntry is a simplified directory entry for the UI
type FileEntry struct {
	Name  string
	Size  int64
	IsDir bool
	Mode  os.FileMode
}

// NewSFTPClient opens an SFTP subsystem over an existing ssh.Client
func NewSFTPClient(sshClient *ssh.Client) (*SFTPClient, error) {
	c, err := sftp.NewClient(sshClient)
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	return &SFTPClient{client: c}, nil
}

// ListDir returns the contents of a remote directory
func (s *SFTPClient) ListDir(path string) ([]FileEntry, error) {
	infos, err := s.client.ReadDir(path)
	if err != nil {
		return nil, err
	}
	entries := make([]FileEntry, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, FileEntry{
			Name:  info.Name(),
			Size:  info.Size(),
			IsDir: info.IsDir(),
			Mode:  info.Mode(),
		})
	}
	return entries, nil
}

// Download copies a remote file to a local destination path
func (s *SFTPClient) Download(remotePath, localPath string) error {
	remote, err := s.client.Open(remotePath)
	if err != nil {
		return fmt.Errorf("open remote %s: %w", remotePath, err)
	}
	defer remote.Close()

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}
	local, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create local %s: %w", localPath, err)
	}
	defer local.Close()

	_, err = io.Copy(local, remote)
	return err
}

// Upload copies a local file to a remote destination path
func (s *SFTPClient) Upload(localPath, remotePath string) error {
	local, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local %s: %w", localPath, err)
	}
	defer local.Close()

	remote, err := s.client.Create(remotePath)
	if err != nil {
		return fmt.Errorf("create remote %s: %w", remotePath, err)
	}
	defer remote.Close()

	_, err = io.Copy(remote, local)
	return err
}

// Delete removes a remote file or empty directory
func (s *SFTPClient) Delete(remotePath string) error {
	return s.client.Remove(remotePath)
}

// Mkdir creates a remote directory
func (s *SFTPClient) Mkdir(remotePath string) error {
	return s.client.MkdirAll(remotePath)
}

// Rename moves/renames a remote path
func (s *SFTPClient) Rename(oldPath, newPath string) error {
	return s.client.Rename(oldPath, newPath)
}

// Getwd returns the remote working directory
func (s *SFTPClient) Getwd() (string, error) {
	return s.client.Getwd()
}

// Close closes the SFTP connection
func (s *SFTPClient) Close() {
	if s.client != nil {
		s.client.Close()
	}
}
