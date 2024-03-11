---- THIS IS A CLONE OF THE ORIGINAL REPOSITORY ON git.bashclub.org ----

# zsync

ZFS replication script by Thorsten Spille <thorsten@spille-edv.de>
- replicates ZFS filesystems/volumes with user parameter bashclub:zsync (or custom name) configured
- creates optional snapshot before replication (required zfs-auto-znapshot)
- parameter setting uses zfs hierarchy on source
- mirrored replication with existing snapshots (filtered by snapshot_filter)
- pull/local replication only
- auto creates full path on target pool, enforce com.sun:auto-snapshot=false, inherits mountpoint and sets canmount=noauto
- raw replication
- tested on Proxmox VE 7.x/8.x
- ssh cipher auto selection

## Installation

#### Install via Repository (Debian / Ubuntu / Proxmox)
~~~
echo "deb [signed-by=/usr/share/keyrings/bashclub-archive-keyring.gpg] https://apt.bashclub.org/release bookworm main" > /etc/apt/sources.list.d/bashclub.list
wget -O- https://apt.bashclub.org/gpg/bashclub.pub | gpg --dearmor > /usr/share/keyrings/bashclub-archive-keyring.gpg
apt update
apt install bashclub-zsync
~~~

#### Download and make executable
~~~
wget -q --no-cache -O /usr/bin/bashclub-zsync https://gitlab.bashclub.org/bashclub/zsync/-/raw/main/bashclub-zsync/usr/bin/bashclub-zsync
chmod +x /usr/bin/bashclub-zsync
bashclub-zsync
~~~

## Documentation
[DOCUMENTATION_DE.md](DOCUMENTATION_DE.md)


## Configuration
After first execution adjust the default config file `/etc/bashclub/zsync.conf`:

~~~
# replication target path on local machine 
target=pool/dataset

# ssh address of remote machine
source=user@host

# ssh port of remote machine
sshport=22

# zfs user parameter to identify filesystems/volumes to replicate
tag=bashclub:zsync

# pipe separated list of snapshot name filters
snapshot_filter="hourly|daily|weekly|monthly"

# minimum count of snapshots per filter to keep
min_keep=3

# number of zfs snapshots to keep on source (0 or 1 = snapshot function disabled)
zfs_auto_snapshot_keep=0

# make snapshot via zfs-auto-snapshot before replication
zfs_auto_snapshot_label="backup"

# disable checkzfs with value > 0
checkzfs_disabled=0

# set checkzfs parameter "--prefix"
checkzfs_prefix=zsync

# set checkzfs maximum age of last snapshot in minutes (comma separated => warn,crit)
checkzfs_max_age=1500,6000

# set checkzfs maximum count of snapshots per dataset (comma separated => warn,crit)
checkzfs_max_snapshot_count=150,165
~~~

### Define a cronjob
#### cron.d example
File: /etc/cron.d/bashclub-zsync
~~~
00 23 * * * root /usr/bin/bashclub-zsync -c /etc/bashclub/zsync.conf > /var/log/bashclub-zsync/zsync.log
~~~

#### cron.{hourly|daily|weekly|monthly}
File: /etc/cron.hourly/bashclub-zsync
~~~
/usr/bin/bashclub-zsync -c /etc/bashclub/zsync.conf > /var/log/bashclub-zsync/zsync.log
~~~

# Author

### Thorsten Spille
[<img src="https://storage.ko-fi.com/cdn/brandasset/kofi_s_tag_dark.png" rel="Support me on Ko-Fi">](https://ko-fi.com/thorakel)
