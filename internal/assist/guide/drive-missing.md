# Apps or Docker are on a drive that's missing
Signs: drive missing, unplugged, not mounted, mount, storage, 8TB, external drive, apps gone after restart, docker empty, fstab
Applies: linux

## Why
Things moved to another drive on the Storage page live there: an app's folder is a link to it, and Docker's images
are mounted from it. If the drive is unplugged, fails, or doesn't mount at startup, they're missing. Docker and
containerd then don't start at all (rather than start empty), and apps on it don't run. (The AI models always stay on
the system drive.)

## Fix
1. `run_checks` lists what's missing and from where.
2. Plug the drive back in. On the server's console, `sudo mount -a` mounts everything in /etc/fstab; then
   `sudo systemctl start containerd docker`, and start the apps (Apps page, or `restart_app`).
3. Homeport's lines in /etc/fstab start with "# Homeport:"; a copy of the file before each change is kept as
   /etc/fstab.homeport-backup.
4. The drive failed: restore the apps from a backup (Backups page) onto another drive.
