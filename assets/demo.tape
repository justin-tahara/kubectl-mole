# VHS tape for the README demo (https://github.com/charmbracelet/vhs).
# Re-record with: make demo (needs the mole-dev kind cluster; the target
# stages the shop scenario and generates .kube/demo-config, which aliases
# the demo cluster under the context names prod-east and prod-west — the
# multi-cluster scene shows exactly the output shape real distinct
# clusters produce).
Output assets/demo.gif

Set Theme "Catppuccin Mocha"
Set FontSize 16
Set Width 1240
Set Height 560
Set Padding 16
Set TypingSpeed 40ms

Hide
Type "export KUBECONFIG=$PWD/.kube/demo-config PATH=$PWD/bin:$PATH PS1='$ '"
Enter
Type "clear"
Enter
Show

# Scene 1: the raw view, then the verdict, then the exit code.
Type "kubectl get pods -n shop"
Enter
Sleep 3.5s

Type "kubectl mole deployment/checkout -n shop --timeout 12s"
Enter
Sleep 14s

Type "echo $?"
Enter
Sleep 3.5s

Hide
Type "clear"
Enter
Show

# Scene 2: the whole namespace, across two clusters, in one verdict —
# per-context rollup, healthy workloads counted, one collapsed cause.
Type "kubectl mole -n shop --contexts prod-east,prod-west --wedged-for 8s"
Enter
Sleep 24s

Sleep 5s
