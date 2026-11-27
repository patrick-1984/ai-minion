package filexfer

// haveLinkFallback: Linux and Android publish with link+unlink where the file
// system does not support RENAME_NOREPLACE; the tests force that path.
const haveLinkFallback = true

func setLinkFallback(v bool) { forceLinkFallback = v }
