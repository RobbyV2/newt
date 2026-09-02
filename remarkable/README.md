newt on the reMarkable

Runs newt as a systemd service on the reMarkable 1, 2 and Paper Pro, started at
boot and carried across firmware updates. The Go code is unchanged; the port is
the cross build, the unit and the persistence around it.

    remarkable/build.sh                                    # cross-compiles into remarkable/out
    remarkable/install.sh --id ID --secret S --endpoint https://pangolin.example
    remarkable/install.sh                                  # reuse the config already on the tablet
    HOST=root@192.168.1.20 remarkable/install.sh           # over wifi instead of USB

build.sh needs Go 1.25 or newer. install.sh needs ssh to the tablet; USB gives
root@10.11.99.1. The tablet needs nothing installed beforehand and no wifi for
the install itself.

Layout on the tablet

    /home/root/newt/newt                 the binary for its architecture
    /home/root/newt/config.json          id, secret, endpoint, mode 600
    /home/root/units/newt.service        the unit, source of truth for rm-persist
    /home/root/.local/bin/rm-persist     keeps the unit alive, see below

The unit runs newt in its default netstack mode, which needs no /dev/net/tun,
no kernel WireGuard and no capabilities; the tablets ship without a tun device.
Self-update is off through NEWT_SYSTEM_SUBSTRATE=CONTAINER in the unit, so the
server cannot swap the binary for its own build; drop that line to allow it.

    ssh root@10.11.99.1 systemctl status newt                  # is it up
    ssh root@10.11.99.1 journalctl -u newt -f                  # follow the log
    ssh root@10.11.99.1 /home/root/.local/bin/rm-persist status

Surviving reboots and updates

The firmware keeps two root partitions and flashes the inactive one on every
update, and /home is the only partition that survives. On the Paper Pro /etc is
also a boot-wiped tmpfs overlay over a read-only rootfs. Community tools such as
toltec, vellum and xovi-tripletap solve this with a re-enable step the owner
runs after each update. rm-persist runs that step by itself:

    rm-persist install      stages every /home/root/units/*.service into the
                            live /etc, the running rootfs and the other root
                            partition, then enables them. The rootfs is reached
                            through a non-recursive bind mount of /, which leaves
                            the /etc overlay and the dropbear host-key mount alone.
    swupdate hook           /etc/swupdate/conf.d/50-rm-persist adds -p to the
                            updater, so it runs rm-persist postupdate on the old
                            system right after the new partition is written.
    rm-persist.service      re-stages into the other partition at boot and at
                            shutdown, covering updates applied any other way.

    rm-persist status                   # per-partition check plus swupdate hook state
    rm-persist sync                     # re-stage by hand
    rm-persist remove newt.service      # take one unit out everywhere
    rm-persist uninstall                # take everything out, keep the unit files

The reMarkable 1 and 2 use the same script. Their rootfs is writable, so the
bind mount lands on the real /etc, and the swupdate hook is written whenever the
firmware ships /usr/lib/swupdate/swupdate.sh. Tested on a Paper Pro; the armv7
build for the reMarkable 2 compiles and runs the same install path but has not
been exercised on a device.
