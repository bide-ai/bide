Section: Changed

The `bide check` warning for a quoted `assert` name that contains a backslash sequence now reads as guidance: it says the name is read as written, names the node the line refers to now (the name with its backslash) and says to write the character itself if that was meant, instead of describing how the old reading would have unescaped it. The condition that triggers it is unchanged.
