# OpenWrt package of the Minion thin Worker. The sources come from clients/core and clients/linux: run
# ../prepare.sh first (it copies them into src/), then build with the SDK or the openwrt/gh-action-sdk action.
include $(TOPDIR)/rules.mk

PKG_NAME:=minion-thin
PKG_VERSION:=0.1.0
PKG_RELEASE:=1
PKG_LICENSE:=Apache-2.0
PKG_LICENSE_FILES:=
PKG_MAINTAINER:=Minion maintainers <noreply@users.noreply.github.com>

include $(INCLUDE_DIR)/package.mk

define Package/minion-thin
  SECTION:=net
  CATEGORY:=Network
  TITLE:=Minion thin Worker (terminal sessions for a Minion Hub)
  URL:=https://github.com/patrick-1984/ai-minion
  DEPENDS:=+libmbedtls
endef

define Package/minion-thin/description
  A small Minion Worker in C: connects outbound to a Minion Hub (WebSocket + the Worker protocol 3.0),
  enrolls once with an invitation key and serves shell sessions (PTY) to the Hub. Trust comes from the
  pinned Hub root key, so no certificate on the router ever expires.
endef

define Package/minion-thin/conffiles
/etc/config/minion-thin
endef

define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
	$(CP) ./src/core ./src/linux $(PKG_BUILD_DIR)/
endef

define Build/Compile
	$(MAKE) -C $(PKG_BUILD_DIR)/linux \
		CC="$(TARGET_CC)" CFLAGS="$(TARGET_CFLAGS) $(TARGET_CPPFLAGS)" LDFLAGS="$(TARGET_LDFLAGS)" \
		CORE=../core BUILD=$(PKG_BUILD_DIR)/out MBEDTLS=1 WERROR=
endef

define Package/minion-thin/install
	$(INSTALL_DIR) $(1)/usr/sbin $(1)/etc/init.d $(1)/etc/config $(1)/lib/upgrade/keep.d
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/out/minion-thin $(1)/usr/sbin/minion-thin
	$(INSTALL_BIN) ./files/minion-thin.init $(1)/etc/init.d/minion-thin
	$(INSTALL_CONF) ./files/minion-thin.config $(1)/etc/config/minion-thin
	$(INSTALL_DATA) ./files/minion-thin.keep $(1)/lib/upgrade/keep.d/minion-thin
endef

$(eval $(call BuildPackage,minion-thin))
