TEMPLATE = subdirs
SUBDIRS += \
    bic \
    headers \
    license \
    symbols \
    guiapplauncher \
    compilerwarnings

# This test is not valid for Windows CE
wince*: SUBDIRS -= guiapplauncher

# This test is only valid on linux
!linux*: SUBDIRS -= symbols
