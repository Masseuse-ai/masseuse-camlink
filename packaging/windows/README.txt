Masseuse.ai for your computer (Windows)
=======================================

Masseuse.exe is the whole program, one file. Nothing is installed:
open it from wherever you saved it, and a window opens with a code of
eight letters and numbers to type into the masseuse.ai app on your phone,
then the camera and microphone it will use and the stimulation unit.
Leave the window open while you use it; closing it stops the program.

This folder is where the window unpacked the files it carries inside
itself, the first time it ran. It checks them every time it starts and
writes them again if anything is missing or changed; a new version
unpacks its own set and removes this one. You can delete this folder; it
comes back.

  masseuse-camlink.exe  the connector: the program behind the window that
                    sends the camera and drives the unit; the window runs
                    it, and it runs alone from a terminal too
  ffmpeg.exe        captures and encodes the camera for the connector; it
                    is never started on its own
  units\            the unit driver helpers: programs the connector runs
                    to serve stimulation units whose drivers are not in
                    the open-source connector (camlink-unit-mk312.exe,
                    the ErosTek MK-312BT over its USB serial link)
  README.txt        this file
  LICENSE, NOTICE   the program's licence (Apache License 2.0)
  THIRD_PARTY.md    what ffmpeg.exe is and how it was built
  licenses\         the licence texts that come with ffmpeg (LGPL 2.1) and Opus

Windows may ask before the first start whether to run an app it does not
recognize while the program's publisher is still new to it: "More info",
then "Run anyway". The file is listed, with its checksum, in the release
it came from, and every release is built in public and verifiable:
https://github.com/FemLed/masseuse-camlink (VERIFY.md, "The Windows
package").

Video is encoded by Windows' own H.264 encoder (Media Foundation). The N
editions of Windows ship without it: install Microsoft's "Media Feature
Pack" from Settings > Apps > Optional features, then open the program again.

The program serves the camera and microphone and a stimulation unit
within reach: a TENS unit over this computer's Bluetooth (Windows 10
version 1703 or newer; Bluetooth on in Settings > Bluetooth & devices),
or an ErosTek MK-312BT plugged in over its USB serial cable. Nothing to
set up: the unit switched on, and the window says when it has found it.
To check the unit without a session, from a terminal in this folder:
masseuse-camlink.exe estim probe

The program remembers its pairing and camera choice in
%LOCALAPPDATA%\masseuse-camlink, where this folder is too. To uninstall,
delete Masseuse.exe and that folder.

The connector is the program engineers know as masseuse-camlink, under
the name you see everywhere else, Masseuse.ai; run from a terminal it
takes the same commands and flags (masseuse-camlink.exe -h). Its log is
desktop.log in %LOCALAPPDATA%\masseuse-camlink.
