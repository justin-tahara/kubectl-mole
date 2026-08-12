# VHS tape for the README demo (https://github.com/charmbracelet/vhs).
# Re-record with: make demo (needs the mole-dev kind cluster and the shop/
# checkout crashloop staged — see the demo target in the Makefile).
Output assets/demo.gif

Set Theme "Catppuccin Mocha"
Set FontSize 17
Set Width 1080
Set Height 640
Set Padding 16
Set TypingSpeed 40ms

Hide
Type "export KUBECONFIG=$PWD/.kube/config PATH=$PWD/bin:$PATH PS1='$ '"
Enter
Type "clear"
Enter
Show

Type "kubectl get pods -n shop"
Enter
Sleep 3s

Type "kubectl mole deployment/checkout -n shop --timeout 12s"
Enter
Sleep 14s

Type "echo $?"
Enter
Sleep 4s
