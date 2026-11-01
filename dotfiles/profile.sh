# Shell setup for an SSH login on the NAS. Not run directly: `homelab install`
# adds a few lines to the login profile that set HOMELAB to this checkout and
# source this file. Keeping it here rather than in the profile means it is
# versioned with the CLI and changes with `homelab update`, and the profile
# keeps only the lines that know where the checkout is.
#
# POSIX sh: the QTS admin shell is busybox ash. bash reads it too.
# shellcheck shell=sh

if [ -n "${HOMELAB:-}" ]; then
    # APPENDED, so bin/ can never shadow a system binary; the case guard keeps
    # PATH from growing when the profile is sourced again in one session.
    case ":${PATH}:" in
        *":${HOMELAB}/bin:"*) ;;
        *) PATH="${PATH}:${HOMELAB}/bin"; export PATH ;;
    esac

    # `homelab cd [stack]` has to change this shell's directory, which a child
    # process cannot do, so it is handled here. Everything else goes to the
    # script.
    homelab() {
        if [ "${1:-}" = cd ]; then
            if [ "$#" -gt 2 ]; then
                echo "usage: homelab cd [stack]" >&2
                return 2
            fi
            cd "${HOMELAB}${2:+/$2}" || return
            return 0
        fi
        command homelab "$@"
    }

    hl() { homelab "$@"; }
fi
