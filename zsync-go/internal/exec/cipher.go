package exec

import (
	"context"
	"log/slog"
	"strings"
)

const (
	// CipherAESGCM is the AES-256-GCM cipher, preferred when both sides
	// have hardware AES-NI support.
	CipherAESGCM = "aes256-gcm@openssh.com"
	// CipherChaChaPoly is the ChaCha20-Poly1305 cipher, preferred when
	// hardware AES is not available on both sides.
	CipherChaChaPoly = "chacha20-poly1305@openssh.com"
)

// DetectAESNI checks whether the machine reachable via the given executor
// has hardware AES-NI support. It first determines the OS type, then
// checks the appropriate source for AES flags.
//
// Linux:   grep -m1 -o aes /proc/cpuinfo
// FreeBSD: grep -o AES /var/run/dmesg.boot
func DetectAESNI(ctx context.Context, exec Executor) bool {
	osID := detectOS(ctx, exec)
	slog.Debug("detected OS", "id", osID, "executor", exec.String())

	var out string
	var err error

	switch osID {
	case "freebsd":
		out, err = exec.Run(ctx, "grep", "-o", "AES", "/var/run/dmesg.boot")
	default:
		// Linux and others.
		out, err = exec.Run(ctx, "grep", "-m1", "-o", "aes", "/proc/cpuinfo")
	}

	if err != nil {
		slog.Debug("AES-NI detection failed", "executor", exec.String(), "error", err)
		return false
	}

	result := len(out) > 0
	slog.Debug("AES-NI detection result", "executor", exec.String(), "aes", result)
	return result
}

// NegotiateCipher detects AES-NI on both the local and remote side and
// returns the optimal SSH cipher. If both sides support AES-NI,
// aes256-gcm@openssh.com is returned; otherwise chacha20-poly1305@openssh.com.
func NegotiateCipher(ctx context.Context, local, remote Executor) string {
	localAES := DetectAESNI(ctx, local)
	remoteAES := DetectAESNI(ctx, remote)

	if localAES && remoteAES {
		slog.Info("both sides support AES-NI, using aes256-gcm@openssh.com")
		return CipherAESGCM
	}

	slog.Info("AES-NI not available on both sides, using chacha20-poly1305@openssh.com",
		"local_aes", localAES, "remote_aes", remoteAES)
	return CipherChaChaPoly
}

// detectOS reads /etc/os-release to determine the OS ID.
// Returns a lowercase string like "debian", "freebsd", "ubuntu", etc.
// Returns "" on failure.
func detectOS(ctx context.Context, exec Executor) string {
	out, err := exec.Run(ctx, "grep", "-E", "^ID=", "/etc/os-release")
	if err != nil {
		return ""
	}
	// Format: ID=debian or ID="ubuntu"
	parts := strings.SplitN(out, "=", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(parts[1]), "\"")
}
