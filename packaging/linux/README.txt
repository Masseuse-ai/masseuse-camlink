Masseuse.ai for your computer (Linux)

This directory is the whole program. Run ./Masseuse for the window, which
shows the pairing code, the cameras and the stimulation unit and runs the
connector behind it; or run ./masseuse-camlink alone in a terminal.
sh install.sh puts Masseuse.ai in the applications menu (sh install.sh -u
takes it out); the program stays in this directory and updates itself here
(README.md, "Updates").

The window needs GTK 4 and WebKitGTK 6.0: on Debian and Ubuntu
  sudo apt install libgtk-4-1 libwebkitgtk-6.0-4
on Fedora
  sudo dnf install gtk4 webkitgtk6.0
To send the camera, the connector needs ffmpeg: sudo apt install ffmpeg
(or your distribution's package). The unit driver helpers are in units/.

Source, releases and how to verify this download: github.com/FemLed/masseuse-camlink
