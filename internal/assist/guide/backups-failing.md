# Backups fail or haven't run
Signs: backup failed, restic, repository, sftp, S3, bucket, wrong password, no backup, snapshot
Applies: any

## Why
Backups use restic. Common causes: the destination is unreachable (server off, drive not plugged in), the SSH key
isn't on the backup server, a bucket that doesn't exist, the backup password changed, or a full destination.

## Fix
1. `backup_status`: the destinations, the last log, and when it last ran.
2. The last log's error, in plain words:
   - "connection refused / no route": the destination is off or unreachable from here.
   - "permission denied (publickey)": add Homeport's key (Backups page) to the backup server's authorized_keys.
   - "wrong password or no key found": the destination has backups made with another password (Backups page →
     "Use the password of earlier backups").
   - "bucket does not exist": create it, or let Homeport create it with the right permissions.
3. The Backups page's "Back up now" button runs one at once; the result shows there.
