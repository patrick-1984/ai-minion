package filexfer

// haveLinkFallback: macOS always publishes with renameatx_np RENAME_EXCL.
const haveLinkFallback = false

func setLinkFallback(bool) {}
