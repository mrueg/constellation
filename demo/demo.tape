# Recording:  vhs demo/demo.tape
#
# Needs a GitHub token (gh auth login is enough to read), and an account with
# no lists on it, so that applying has real work to do.
#
# Each command is filmed being typed, then Hide stops the camera while it runs
# and Show brings it back on the finished output. The command still executes in
# full — what is skipped is the part where a viewer would be watching a
# progress bar. That is most of the running time: without it the recording is
# half as long and every frame is worth reading.

Output demo/constellation.gif

Set Shell bash
Set FontSize 14
Set Width 1000
Set Height 560
Set Padding 20
# The GIF encoder buffers every frame to build its palette, so the frame rate
# and the length of the recording together decide whether ffmpeg fits in
# memory: a minute at 25fps was enough to have it killed for using four
# gigabytes.
Set Framerate 10
Set TypingSpeed 45ms
Set Theme "Catppuccin Mocha"

# Colour is forced because vhs captures through a pipe, where the tool
# correctly turns it off. CONSTELLATION_SHOW keeps the category listing short
# without putting a --show flag in every command on screen.
Hide
Type "export CLICOLOR_FORCE=1 CONSTELLATION_SHOW=2 PS1='$ '"  Enter
Ctrl+L
Type "clear"  Enter
Show

# 1. Group the stars: read them through the API, cluster, write a plan.
#    --show keeps the terminal readable; the plan file holds all 32.
Type "constellation plan"  Sleep 400ms  Enter
Hide
Sleep 17s
Show
Sleep 9s

Hide
Type "export CLICOLOR_FORCE=1 CONSTELLATION_SHOW=2 PS1='$ '"  Enter
Ctrl+L
Type "clear"  Enter
Show

# 2. Create one list and file its repositories, for real. The account is reset
#    before recording so this shows the tool working rather than confirming
#    work already done.
Type "constellation apply --only 'Terraform' --yes"  Sleep 400ms  Enter
Hide
Sleep 14s
Show
Sleep 10s


Hide
Type "export CLICOLOR_FORCE=1 CONSTELLATION_SHOW=2 PS1='$ '"  Enter
Ctrl+L
Type "clear"  Enter
Show
# 3. What the account holds now.
Type "constellation show"  Sleep 400ms  Enter
Sleep 9s
