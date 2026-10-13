# The Linux installer freezes after the sudo password
Signs: installer frozen, hangs, sudo password, curl | sudo bash, Ubuntu 26.04, sudo-rs, stopped
Applies: linux

## Why
Ubuntu 26.04's sudo (sudo-rs) pauses programs started under `curl … | sudo bash` that touch the terminal (tee, stty).
Installers before 0.7.6 used them and froze right after the password.

## Fix
Use the current installer (0.7.6 or later): it doesn't use tee or stty. If an old one froze, press Ctrl+C and run
the current installer again; it picks up where the old one stopped.
