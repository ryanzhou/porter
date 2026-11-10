Porter travel router - boot partition

Everything is configured from the Porter app over Bluetooth. There is
nothing to edit here for normal use.

Debug access: create an empty file named "porter-debug" on this partition
to get a root shell on an attached HDMI display and keyboard, and on the
serial console (on a Pi 5 that is the 3-pin debug connector between the
HDMI ports, 115200 baud). Delete the file to turn it off.
