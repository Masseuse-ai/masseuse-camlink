Masseuse.ai for your computer (Windows)
=======================================

This folder is the whole program. Nothing is installed.

  Masseuse.ai.exe   the program: open it, and a window opens with the
                    camera and microphone it will use and a code of eight
                    letters and numbers to type into the masseuse.ai app on
                    your phone. Leave the window open while you use it;
                    closing it stops the program.
  ffmpeg.exe        captures and encodes the camera for it; it stays beside
                    Masseuse.ai.exe and is never started on its own.
  README.txt        this file
  LICENSE, NOTICE   the program's licence (Apache License 2.0)
  THIRD_PARTY.md    what ffmpeg.exe is and how it was built
  licenses/         the licence texts that come with ffmpeg (LGPL 2.1) and Opus

The first time, Windows may say it protected your PC from an app it does
not recognize: this program is not yet signed with a Windows certificate.
Choose "More info", then "Run anyway". The file you downloaded is listed,
with its checksum, in the release it came from, and every release is
built in public and verifiable: https://github.com/FemLed/masseuse-camlink
(VERIFY.md, "The Windows package").

Video is encoded by Windows' own H.264 encoder (Media Foundation). The N
editions of Windows ship without it: install Microsoft's "Media Feature
Pack" from Settings > Apps > Optional features, then open the program again.

The program serves the camera and microphone and, over this computer's
Bluetooth, a TENS unit within reach (Windows 10 version 1703 or newer).
Nothing to set up: Bluetooth on in Settings > Bluetooth & devices, the unit
switched on, and the window says when it has found it. To check the unit
without a session, from a terminal in this folder: Masseuse.ai.exe estim probe

The program remembers its pairing and camera choice in
%LOCALAPPDATA%\masseuse-camlink. To move it, move the whole folder. To
uninstall, delete the folder (and that one, if you like).

Masseuse.ai.exe is the same program engineers know as masseuse-camlink,
under the name you see everywhere else; run from a terminal it takes the
same commands and flags (Masseuse.ai.exe -h).
