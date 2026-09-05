#!/bin/sh
# autorun.sh - hook the repo's profile.sh into /etc/profile at every boot.
#
# This file is NOT run from the repo. bootstrap.sh copies it to the QNAP config
# partition, which is the only writable-and-persistent place QTS reads at boot.
# Everything else it could touch (/etc, /root, /usr, /opt) is rebuilt from
# firmware on the way up, so a change made here at boot is the only kind that
# survives.
#
# Keep it this small on purpose: changing it means re-running bootstrap.sh to
# re-copy it onto the config partition, which is a step that gets forgotten.
# Anything that might need to change belongs in profile.sh, which is read live
# from the checkout at every login.
#
# The append is safe to do unconditionally: /etc/profile is regenerated from
# firmware on every boot, so there is nothing here to accumulate. It is also
# safe when the data volume is not mounted yet - the volume here is encrypted
# (CE_ prefix) and unlocks on its own schedule, so the guard is evaluated at
# LOGIN time, not now. A boot that races the unlock still produces a working
# /etc/profile; the banner simply appears once the volume is up.

HOMELAB=/share/CE_CACHEDEV4_DATA/homelab

echo "[ -f $HOMELAB/qnap/profile.sh ] && . $HOMELAB/qnap/profile.sh" >> /etc/profile
