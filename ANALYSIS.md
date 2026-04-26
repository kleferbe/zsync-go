# bashclub-zsync – Vollständige Analyse für Go-Portierung

## 1. Überblick

**bashclub-zsync** ist ein ZFS-Replikationstool, das auf dem Zielserver (Backup-Host) läuft und ZFS-Datasets/Volumes von einem Quellserver per `zfs send/receive` repliziert – entweder remote via SSH oder lokal. Es wird typischerweise als Cronjob betrieben.

### Architektur-Diagramm

```
┌─────────────────────────────────────────────────────────────────────────┐
│  ZIELSERVER (führt bashclub-zsync aus)                                 │
│                                                                         │
│  ┌──────────────┐    ┌───────────────┐    ┌──────────────────────────┐ │
│  │ Config lesen  │───►│  Source-Tags   │───►│  Pro Dataset:            │ │
│  │ (zsync.conf)  │    │  abfragen     │    │  1. Initial-Replikation  │ │
│  └──────────────┘    │  (via SSH)     │    │  2. Inkrementell senden  │ │
│                       └───────────────┘    │  3. Alte Snaps löschen   │ │
│                                             └──────────────────────────┘ │
│                                                         │                │
│                                             ┌───────────▼──────────────┐ │
│                                             │  checkzfs (Monitoring)   │ │
│                                             └──────────────────────────┘ │
└──────────────────────┬──────────────────────────────────────────────────┘
                       │ SSH (zfs send | zfs receive)
                       ▼
┌──────────────────────────────────────────────────────────────────────────┐
│  QUELLSERVER                                                             │
│  - ZFS-Datasets mit Tag "bashclub:zsync" = all|subvols|exclude           │
│  - Snapshots (z.B. von zfs-auto-snapshot oder remote-snapshot-manager)   │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Komponenten

| Datei | Typ | Zweck |
|---|---|---|
| `usr/bin/bashclub-zsync` | Bash (Hauptscript) | ZFS-Replikation: Snapshots erkennen, initial/inkrementell replizieren, alte Snapshots bereinigen, checkzfs ausführen |
| `usr/bin/remote-snapshot-manager` | Bash (Hilfstool) | Snapshots auf der Quelle erstellen und alte löschen (Alternative zu `zfs-auto-snapshot`) |
| `usr/bin/zsync-autorun` | Bash (Hilfstool) | Workflow für Replikation auf Wechseldatenträger (LUKS entschlüsseln → ZPool importieren → Replikation → Scrub → Export → Auswerfen) |
| `usr/bin/bashclub-zsync-config` | Bash (unfertig) | Config-Tool zum Auflisten/Setzen von ZFS-Tags auf der Quelle |
| `usr/bin/bashclub-zsync.py` | Python (unfertig) | Begonnene Python-Portierung, nur Grundstruktur |
| `etc/bashclub/zsync.conf.example` | Config | Beispielkonfiguration |
| `etc/systemd/system/zsync-autorun@.service` | Systemd Unit | Template-Service für zsync-autorun |
| `etc/udev/rules.d/90-automount-zsync.rules.example` | udev Rule | Trigger für zsync-autorun bei USB-Disk-Einstecken |
| `etc/logrotate.d/bashclub-zsync` | Logrotate | Wöchentliche Log-Rotation, 12 Wochen aufbewahren |
| `DEBIAN/*` | Packaging | Debian-Paketierung (preinst, postinst, prerm, postrm) |

---

## 3. Konfiguration (`zsync.conf`)

Die Config-Datei ist ein einfaches Shell-Script (key=value), das per `source` eingelesen wird.

### Alle Konfigurationsparameter

| Parameter | Default | Beschreibung |
|---|---|---|
| `target` | `pool/dataset` | ZFS-Zielpfad auf dem Backupserver, unter dem die replizierten Datasets angelegt werden |
| `source` | `user@host` | SSH-Adresse des Quellservers. **Leer = Lokalmodus** (kein SSH) |
| `sshport` | `22` | SSH-Port des Quellservers |
| `tag` | `bashclub:zsync` | ZFS User-Property auf der Quelle, das Datasets zur Replikation markiert |
| `snapshot_filter` | `hourly\|daily\|weekly\|monthly` | Pipe-separierte Liste von Snapshot-Namensfiltern (Regex-Alternation) |
| `min_keep` | `3` | Mindestanzahl Snapshots pro Filter-Intervall auf dem Ziel |
| `zfs_auto_snapshot_keep` | `0` | Anzahl zu behaltender Snapshots bei Auto-Snapshot. 0 oder 1 = Funktion deaktiviert |
| `zfs_auto_snapshot_label` | `backup` | Label für den Auto-Snapshot (ergibt z.B. `zfs-auto-snap_backup-2024-01-15-1430`) |
| `zfs_auto_snapshot_engine` | `zas` | Snapshot-Engine: `zas` = zfs-auto-snapshot, `internal` = remote-snapshot-manager |
| `checkzfs_disabled` | `0` | checkzfs deaktivieren (>0) |
| `checkzfs_local` | `0` | checkzfs nur lokal ausführen (kein --source Parameter) |
| `checkzfs_prefix` | `zsync` | Prefix für checkzfs-Output |
| `checkzfs_max_age` | `1500,6000` | Max-Alter des letzten Snapshots in Minuten (warn,crit) |
| `checkzfs_max_snapshot_count` | `150,165` | Max-Snapshot-Anzahl pro Dataset (warn,crit) |
| `checkzfs_spool` | `0` | Wohin checkzfs-Output: 0 = lokal, 1 = Quellserver |
| `checkzfs_spool_maxage` | `87000` | Max-Alter der checkzfs-Spooldaten in Sekunden |

### ZFS-Tag-Werte auf der Quelle

Das Property (default `bashclub:zsync`) wird auf ZFS-Datasets der Quelle gesetzt:

| Wert | Bedeutung |
|---|---|
| `all` | Repliziert das Dataset und alle Kinder |
| `subvols` | Repliziert nur die Kind-Datasets (nicht das Dataset selbst, auf dem das Tag gesetzt ist) |
| `exclude` | Schließt das Dataset explizit von der Replikation aus |

Zusätzlich wird die `source`-Spalte von `zfs get` ausgewertet:
- **`local`**: Tag wurde direkt auf diesem Dataset gesetzt
- **`received`**: Tag wurde durch Replikation empfangen → wird ignoriert (verhindert Schleifen)
- **`inherited`**: Tag wurde geerbt → bei `subvols` wird eingeschlossen, bei `all` wird eingeschlossen

---

## 4. Ablauf des Hauptscripts (`bashclub-zsync`)

### Phase 0: Initialisierung (Zeilen 1–148)
!! Bitte statt externer Tools golang-Bibliotheken verwenden, falls in guter Qualität vorhanden !!
```
1. Externe Programme lokalisieren (zfs, ssh, scp, grep, etc.)
2. Defaults für alle Konfigurationsparameter setzen
3. CLI-Argumente parsen (-h, -d, -c)
4. checkzfs-Verfügbarkeit prüfen
5. Config-Datei laden (source) oder erstellen falls nicht vorhanden
```
!! Die Configuration soll als yaml-Datei umgesetzt werden !!

### Phase 1: SSH-Setup & Cipher-Auswahl (Zeilen 150–195)

```
1. Lokalmodus erkennen (source == "")
   → SSH-Variablen leeren
2. SSH ControlMaster konfigurieren (Connection Multiplexing):
   - ControlMaster=auto
   - ControlPath=~/.ssh/bashclub-zsync-%r@%h-%p
   - ControlPersist=15
3. OS-Erkennung (Linux vs FreeBSD) auf beiden Seiten
4. AES-HW-Support prüfen (/proc/cpuinfo bzw. dmesg.boot)
   → AES vorhanden: aes256-gcm@openssh.com
   → Kein AES:      chacha20-poly1305@openssh.com
```

### Phase 2: Dataset-Erkennung (Zeilen 196–234)
```
1. Auf der Quelle via SSH: `zfs get -H -o name,value,source -t filesystem,volume $tag`
2. Für jedes Dataset auswerten:
   - tag-value == "subvols":
     - source != "local" und != "received" → in syncvols[] aufnehmen
     - source == "local" → in checkzfs_filter aufnehmen (Format: #name/)
   - tag-value == "all":
     - source != "received" → in syncvols[] aufnehmen
     - source == "local" → in checkzfs_filter aufnehmen (Format: #name)
   - Sonstiges (inkl. "exclude") → exclude_list (nur Logging)
3. Ergebnis: Array syncvols[] mit allen zu replizierenden Datasets
```

### Phase 3: Ziel-Dataset vorbereiten (Zeilen 236–244)

```
1. Prüfen ob $target existiert
   → Nein: `zfs create -o canmount=noauto -o com.sun:auto-snapshot=false $target`
   → Ja: Sicherstellen dass com.sun:auto-snapshot=false gesetzt ist
```

### Phase 4: Optionale Snapshot-Erstellung auf Quelle (Zeilen 246–256)
!! Wird nicht benötigt. Wir gehen davon aus, dass Snapshots auf der Quelle vorhanden und extern verwaltet sind. nicht nach golang portieren. !!
```
Wenn zfs_auto_snapshot_keep > 1:
  - Engine "zas": `zfs-auto-snapshot --quiet --syslog --label=$label --keep=$keep //`
  - Engine "internal": wird später pro Dataset in Phase 5 aufgerufen
  - snapshot_filter um das Label erweitern
```

### Phase 5: Replikationsschleife (Zeilen 258–336) – **Kernlogik**

Für jedes Dataset in `syncvols[]`:

#### 5a. Typ-Erkennung
```
fstype = `zfs get type $name`  (auf Quelle via SSH)
→ filesystem: -xmountpoint und -ocanmount=noauto Flags setzen
→ volume: keine zusätzlichen Flags
```

#### 5b. Optionale Snapshot-Erstellung (Engine "internal")
!! Wird nicht benötigt. Wir gehen davon aus, dass Snapshots auf der Quelle vorhanden und extern verwaltet sind. nicht nach golang portieren. !!

```
Wenn zfs_auto_snapshot_keep > 1 und engine == "internal":
  → remote-snapshot-manager aufrufen (-d $name -k $keep -l $label -h $source -p $sshport)
```

#### 5c. Snapshot-Existenzprüfung

```
Auf Quelle prüfen: Gibt es mindestens 1 Snapshot, der dem snapshot_filter entspricht?
→ Nein: Dataset überspringen
→ Ja: weiter
```

#### 5d. Initiale Replikation (wenn Ziel noch nicht existiert)

```
1. $target/$name existiert nicht lokal:
2. Hierarchiepfad erstellen: Für jedes Segment von $target/$poolname
   → `zfs create -o canmount=noauto -p $segment` (falls nicht vorhanden)
3. Ältesten passenden Snapshot auf Quelle finden:
   `zfs list -H -t snapshot -o name -S creation $name | grep -E "@.*($filter)" | tail -1`
4. Full Send:
   `ssh source "zfs send -w -p $snap" | zfs receive -xmountpoint -ocanmount=noauto -x$tag -xauto-snapshot -dF $target/$poolname`
   
   Flags:
   - `-w`: raw send (encrypted datasets bleiben verschlüsselt)
   - `-p`: Properties mitsenden
   - `-d`: Auf Empfängerseite den Pool-Teil des Namens ersetzen
   - `-F`: Force receive (Rollback wenn nötig)
   - `-x $tag`: Tag-Property nicht empfangen
   - `-x com.sun:auto-snapshot`: auto-snapshot Property nicht empfangen
```

#### 5e. Inkrementelle Replikation

```
1. GUID des neuesten Snapshots auf dem Ziel ermitteln:
   `zfs list -H -o guid -s creation -t snapshot $target/$name | tail -1`
2. Auf der Quelle den Snapshot mit dieser GUID finden:
   `ssh source "zfs list -H -o name,guid -t snapshot $name | grep $guid | tail -1 | cut -f1"`
3. Falls gefunden → Alle neueren Snapshots (nach dem GUID-Match) replizieren:
   `ssh source "zfs list ... | grep -E @.*($filter) | grep --after-context=200 $guid | grep -v $guid | cut -f1"`
4. Für jeden neuen Snapshot:
   `ssh source "zfs send -w -i $last $snap" | zfs receive -x$tag -xauto-snapshot -F $target/$name`
   → `-i $last $snap`: inkrementeller Send (Delta zwischen last und snap)
5. Fehlerbehandlung:
   → GUID nicht auf Quelle gefunden = gemeinsamer Basis-Snapshot fehlt → ERROR
   → Kein Snapshot auf Ziel = Ziel muss gelöscht und neu repliziert werden → ERROR
```

#### 5f. Alte Snapshots auf Ziel löschen

```
Für jedes Intervall im snapshot_filter (hourly, daily, weekly, monthly):
1. GUID des ältesten Intervall-Snapshots auf der QUELLE finden
   → `ssh source "zfs list ... -S creation | grep @.*$interval | cut -f1 | tail -1"`
   (Achtung: -S creation = absteigend sortiert → tail -1 = ältester)
2. Auf dem ZIEL: Alle Snapshots dieses Intervalls finden, die ÄLTER sind als dieser GUID
   → grep --after-context=200 $guid | grep -v $guid
3. Solange snap_count > min_keep: `zfs destroy $snap`
```

### Phase 6: checkzfs (Monitoring) (Zeilen 338–360)
```
1. checkzfs aufrufen mit allen konfigurierten Parametern
2. Output in temporäre Datei schreiben
3. Je nach checkzfs_spool:
   → 0 oder checkzfs_local: In lokales Checkmk Spool-Dir verschieben
   → 1: Per SCP auf Quellserver übertragen
```

---

## 5. `remote-snapshot-manager` (Alternative Snapshot-Engine)
!! Nicht in golang portieren. Wird nicht benötigt. Alle Snapshots werden mit bereits konfiguriertem und bewährten zfs-auto-snapshot unabhängig von der Backup-Synchronisation auf der Quelle erstellt und verwaltet. !!
Eigenständiges Script, das auf der Quelle (via SSH) Snapshots erstellt und alte löscht.

### Parameter
| Flag | Beschreibung |
|---|---|
| `-d` | ZFS-Dataset-Name |
| `-k` | Anzahl zu behaltender Snapshots |
| `-l` | Label für den Snapshot |
| `-f` | Datumsformat (Default: `%Y-%m-%d-%H%M`) |
| `-v` | Verbose/Debug-Modus |
| `-h` | Remote-Host (SSH) |
| `-p` | SSH-Port |

### Ablauf

```
1. SSH-Verbindung testen (falls Remote)
2. Snapshot erstellen: ${dataset}@${label}-$(date -u +"$dateformat")
3. Vorhandene Snapshots mit gleichem Label auflisten
4. Wenn Anzahl > keep: Älteste löschen
```

---

## 6. `zsync-autorun` (Wechseldatenträger-Workflow)
!! Der Wechseldatenträger-Workflow wird nicht benötigt und soll nicht zu golang portiert werden. !!

Für Offline-Backups auf USB-Festplatten mit optionaler LUKS-Verschlüsselung.

### Ablauf

```
1. Config-Datei einlesen (aus /etc/default/zsync-autorun@$pool)
2. Pool-Name aus target extrahieren
3. Disk per blkid identifizieren
4. Falls LUKS: Entschlüsseln mit Keyfile (/etc/bashclub/${pool}.key)
5. ZPool importieren
6. bashclub-zsync mit -d -c ausführen
7. Am Wochenende (Fr-So): zpool scrub laufen lassen
8. zpool sync + export
9. Falls LUKS: cryptsetup close
10. Disk per udisksctl auswerfen
11. Status-Mail senden
```

### Trigger

- **udev-Rule**: Erkennt USB-Disk anhand der ZFS Pool-GUID → startet `zsync-autorun@<pool>.service`
- **Systemd-Unit**: Template-Service, der `/usr/bin/zsync-autorun /etc/default/zsync-autorun@%i` aufruft

---

## 7. SSH-Details

### Connection Multiplexing

```
-oControlMaster=auto
-oControlPath=~/.ssh/bashclub-zsync-%r@%h-%p
-oControlPersist=15
```

Hält die SSH-Verbindung 15 Sekunden offen für Wiederverwendung. Reduziert den Overhead bei vielen aufeinanderfolgenden SSH-Aufrufen erheblich.

### Cipher-Auswahl

Prüft ob beide Seiten AES-Hardwarebeschleunigung haben:
- **Ja**: `aes256-gcm@openssh.com` (schnellster mit HW-AES)
- **Nein**: `chacha20-poly1305@openssh.com` (schnellster ohne HW-AES)

### ZFS-Berechtigungen (zfs_allow)

| Seite | Benötigte `zfs allow`-Berechtigungen |
|---|---|
| Target (Empfänger) | `create, destroy, receive, userprop, canmount, mountpoint` |
| Source (Sender) | `send, snapshot` |

---

## 8. Identifizierte Bugs und Probleme

### Im Bash-Script (`bashclub-zsync`)

1. **Zeile 121**: Typo in Log-String: `\ŧ` statt `\t` (Unicode ŧ statt Tab)
2. **GUID-Matching ist fragil**: `grep $guid` ohne Wortgrenzen könnte bei Teil-Matches fehlschlagen
3. **`grep --after-context=200`**: Hardcoded Limit von 200 nachfolgenden Zeilen – bei >200 Snapshots werden welche ignoriert
4. **Kein Locking**: Mehrfache gleichzeitige Ausführung möglich
5. **Fehlende Fehlerbehandlung bei SSH-Verbindungsabbrüchen**: Kein Retry, kein Cleanup
6. **`checkzfs_filter` Trailing-Pipe entfernen**: `${checkzfs_filter::-1}` schlägt fehl, wenn der Filter leer ist
7. **Bei leerem `source`**: SSH-Commands werden trotzdem zusammengebaut (leerer SSH-Aufruf)
8. **IFS-Wechsel**: `IFS=$'\n'` und `IFS=$' '` werden innerhalb der Schleife gesetzt, aber nie zurückgesetzt → kann Seiteneffekte haben

### Im `bashclub-zsync-config`

- **Bug**: Zeile `value=$(echo $OPTARG | cut -d'=' -f 1)` – verwendet `-f 1` statt `-f 2`, setzt `value` auf den Key statt den Wert
- **Unfertig**: Die `list()`-Funktion hat eine leere `for`-Schleife
- **Syntax-Fehler**: `$recursive -gt0` fehlt Leerzeichen → `-gt 0`

### Im `remote-snapshot-manager`

- **Kein SSH ControlMaster**: Baut bei jedem Aufruf eine neue SSH-Verbindung auf

---

## 9. Datenfluss-Zusammenfassung

```
CONFIG EINLESEN
      │
      ▼
SSH-SETUP (Cipher, ControlMaster)
      │
      ▼
DATASETS ERKENNEN ──── zfs get $tag auf Quelle
      │                (name, value, source)
      │
      ├── value="all" + source≠"received" ──► syncvols[]
      ├── value="subvols" + source≠"local"∧≠"received" ──► syncvols[]
      └── sonst ──► exclude
      │
      ▼
[OPTIONAL] SNAPSHOTS ERSTELLEN ──── zfs-auto-snapshot oder remote-snapshot-manager
      │
      ▼
FÜR JEDES DATASET IN syncvols[]:
      │
      ├── Ziel existiert NICHT?
      │     └── Full Send (ältester passender Snapshot)
      │           ssh "zfs send -w -p @snap" | zfs receive -dF
      │
      ├── Ziel existiert?
      │     ├── GUID des neuesten Ziel-Snapshots ermitteln
      │     ├── Auf Quelle: Snapshot mit dieser GUID finden
      │     ├── Alle neueren Snapshots inkrementell senden
      │     │     ssh "zfs send -w -i @last @snap" | zfs receive -F
      │     └── Fehler wenn GUID nicht auf Quelle gefunden
      │
      └── Alte Snapshots bereinigen
            Pro Intervall (hourly, daily, ...):
              Ältesten Quell-Snapshot-GUID finden
              Auf Ziel: Alles nach dieser GUID löschen (min_keep beachten)
      │
      ▼
CHECKZFS AUSFÜHREN ──── Monitoring-Daten generieren
      │
      ▼
EXIT mit Return-Code (0=OK, 1=Fehler)
```

---

## 10. Hinweise für die Go-Portierung

### Empfohlene Go-Architektur

```
cmd/
  zsync/main.go              # CLI-Einstiegspunkt
  remote-snapshot-manager/    # Eigenes Binary oder Subcommand
  zsync-autorun/              # Eigenes Binary oder Subcommand

internal/
  config/
    config.go                 # Config-Parsing (key=value Format oder YAML/TOML)
  
  zfs/
    zfs.go                    # ZFS-Befehle abstrahieren (list, send, receive, create, destroy, get, set)
    snapshot.go               # Snapshot-Logik (auflisten, filtern, GUID-Matching)
    types.go                  # Dataset, Snapshot, Property Typen
  
  ssh/
    ssh.go                    # SSH-Verbindungsmanagement (ControlMaster-Äquivalent)
    executor.go               # Kommandoausführung lokal oder remote
  
  replication/
    replication.go            # Kernlogik: Initial- und Inkrementelle Replikation
    cleanup.go                # Snapshot-Bereinigung auf dem Ziel
    discovery.go              # Dataset-Erkennung (Tag-Auswertung)
  
  monitoring/
    checkzfs.go               # checkzfs-Integration
  
  autorun/
    autorun.go                # Wechseldatenträger-Workflow (LUKS, Import, Export)
```

### Zentrale Konzepte zu abstrahieren

1. **CommandExecutor Interface**: Lokale vs. Remote (SSH) Kommandoausführung
   ```go
   type CommandExecutor interface {
       Execute(ctx context.Context, cmd string, args ...string) (string, error)
       ExecutePipe(ctx context.Context, sender, receiver Command) error
   }
   ```

2. **ZFS-Abstraktionsschicht**: Statt rohe Shell-Befehle zu parsen
   ```go
   type ZFSClient struct { executor CommandExecutor }
   func (z *ZFSClient) ListSnapshots(dataset string) ([]Snapshot, error)
   func (z *ZFSClient) GetProperty(dataset, property string) (PropertyValue, error)
   func (z *ZFSClient) Send(ctx context.Context, opts SendOptions) (io.Reader, error)
   func (z *ZFSClient) Receive(ctx context.Context, target string, opts ReceiveOptions, r io.Reader) error
   ```

3. **SSH mit golang.org/x/crypto/ssh**: Nativer SSH-Client mit Connection-Pooling statt ControlMaster

4. **Strukturierte Konfiguration**: Statt Shell-Source → TOML, YAML oder INI mit Validierung

5. **Sauberes Locking**: Lockfile oder flock()-Äquivalent

6. **Context und Cancellation**: `context.Context` für Timeout und Abbruch bei SSH-Problemen

7. **Strukturiertes Logging**: `slog` (stdlib) mit Level-Support

### ZFS-Befehle die abstrahiert werden müssen

| Operation | Bash-Aufruf | Anmerkung |
|---|---|---|
| Datasets mit Tag auflisten | `zfs get -H -o name,value,source -t filesystem,volume $tag` | Auf Quelle |
| Snapshots auflisten | `zfs list -H -t snapshot -o name,guid -s creation $dataset` | Sortiert nach Erstellung |
| Dataset-Typ ermitteln | `zfs get -H -o value type $name` | filesystem oder volume |
| Property lesen | `zfs get -H -o value,source $property $dataset` | |
| Property setzen | `zfs set $property=$value $dataset` | |
| Dataset erstellen | `zfs create -o canmount=noauto [-o auto-snapshot=false] [-p] $dataset` | `-p` für Parent-Erstellung |
| Dataset existiert? | `zfs list -H $dataset` | Exit-Code prüfen |
| Full Send | `zfs send -w -p [-v] $snapshot` | Raw, mit Properties |
| Incremental Send | `zfs send -w [-v] -i $from $to` | Delta |
| Receive | `zfs receive [-x prop]... [-o prop=val]... [-xmountpoint] -F [-d] [-v] $target` | |
| Snapshot erstellen | `zfs snapshot $dataset@$name` | |
| Snapshot löschen | `zfs destroy [-v] $snapshot` | |

---

## 11. Testbarkeit

Das Bash-Script ist nicht testbar. Für Go empfehle ich:

1. **Interface-basierte Architektur**: Alle externen Aufrufe (ZFS, SSH) hinter Interfaces
2. **Mock-Implementierungen**: Für Unit-Tests
3. **Integration-Tests**: Mit echtem ZFS (z.B. in VM oder Container mit ZFS-on-Linux)
4. **Testdaten**: Fixtures für `zfs list`/`zfs get` Output zum Parsen
