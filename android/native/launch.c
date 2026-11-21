// Keep supervised executables tied to the Android Service's process without
// depending on an onDestroy callback. The caller keeps its spawning thread
// alive for the lifetime of the supervised children.
#include <signal.h>
#include <stdlib.h>
#include <sys/prctl.h>
#include <unistd.h>

int
main(int argc, char **argv)
{
	char *end;
	long parent;

	if (argc < 3 || argv[2][0] != '/')
		return 64;
	parent = strtol(argv[1], &end, 10);
	if (*end != '\0' || parent <= 1)
		return 64;
	if (prctl(PR_SET_PDEATHSIG, SIGTERM, 0, 0, 0) == -1)
		return 71;
	// The parent may have died before prctl registered the signal.
	if (getppid() != parent)
		return 71;
	execv(argv[2], &argv[2]);
	return 126;
}
