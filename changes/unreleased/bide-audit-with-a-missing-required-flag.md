Section: Fixed

An audit verb run without a required flag, such as `bide audit verify-approvals` without `-call`, `-need` or `-approvers`, printed about 95 lines of usage for every verb and no line saying what was missing. It now prints one line that names the missing flags and says to run `bide audit <verb> -h`, and exits 2 as before. Every audit verb does the same.
